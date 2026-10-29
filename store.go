package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// Item é um filme ou uma série: uma entrada (arquivo ou pasta) no primeiro
// nível da pasta de filmes, que é como o qBittorrent salva cada torrent.
type Item struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`  // nome no disco
	Title    string  `json:"title"` // título limpo, ou o que o usuário escolheu
	Year     string  `json:"year,omitempty"`
	Series   bool    `json:"series"`
	AddedAt  string  `json:"addedAt"`
	HasThumb bool    `json:"hasThumb"`
	Files    []*File `json:"files,omitempty"`
	// Resumo para os cartões.
	Count    int     `json:"count"`    // arquivos de vídeo
	Watched  int     `json:"watched"`  // quantos já foram vistos
	Size     int64   `json:"size"`     // bytes
	Duration float64 `json:"duration"` // segundos (soma)
}

// File é um arquivo de vídeo de um item.
type File struct {
	ID       int64   `json:"id"`
	ItemID   int64   `json:"-"`
	Rel      string  `json:"rel"` // caminho relativo à pasta de filmes
	Label    string  `json:"label"`
	Season   int     `json:"season"`  // 0 quando não é episódio
	Episode  int     `json:"episode"` // 0 quando não é episódio
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
	Watched  bool    `json:"watched"`
}

var ErrNotFound = errors.New("não encontrado")

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS items (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL UNIQUE,
	title       TEXT NOT NULL DEFAULT '', -- título escolhido pelo usuário; vazio usa o do nome
	added_at    TEXT NOT NULL,
	has_thumb   INTEGER NOT NULL DEFAULT 0,
	thumb_tried INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS files (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	rel      TEXT NOT NULL UNIQUE,
	size     INTEGER NOT NULL,
	duration REAL NOT NULL DEFAULT 0,
	watched  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS files_item ON files(item_id);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// itemRow é o que o banco guarda de um item; o resto vem do nome e dos arquivos.
type itemRow struct {
	ID         int64
	Name       string
	Title      string
	AddedAt    string
	HasThumb   bool
	ThumbTried bool
}

func (s *Store) itemRows() (map[string]*itemRow, error) {
	rows, err := s.db.Query(`SELECT id, name, title, added_at, has_thumb, thumb_tried FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*itemRow{}
	for rows.Next() {
		r := &itemRow{}
		if err := rows.Scan(&r.ID, &r.Name, &r.Title, &r.AddedAt, &r.HasThumb, &r.ThumbTried); err != nil {
			return nil, err
		}
		out[r.Name] = r
	}
	return out, rows.Err()
}

func (s *Store) itemRow(id int64) (*itemRow, error) {
	r := &itemRow{}
	err := s.db.QueryRow(`SELECT id, name, title, added_at, has_thumb, thumb_tried FROM items WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Title, &r.AddedAt, &r.HasThumb, &r.ThumbTried)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *Store) addItem(name, addedAt string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO items (name, added_at) VALUES (?, ?)`, name, addedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) deleteItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM items WHERE id = ?`, id)
	return err
}

func (s *Store) SetTitle(id int64, title string) error {
	res, err := s.db.Exec(`UPDATE items SET title = ? WHERE id = ?`, title, id)
	return notFoundIfNone(res, err)
}

func (s *Store) setThumb(id int64, ok bool) error {
	_, err := s.db.Exec(`UPDATE items SET has_thumb = ?, thumb_tried = 1 WHERE id = ?`, ok, id)
	return err
}

// fileRows devolve os arquivos já conhecidos, pelo caminho relativo.
func (s *Store) fileRows() (map[string]*File, error) {
	rows, err := s.db.Query(`SELECT id, item_id, rel, size, duration, watched FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*File{}
	for rows.Next() {
		f := &File{}
		if err := rows.Scan(&f.ID, &f.ItemID, &f.Rel, &f.Size, &f.Duration, &f.Watched); err != nil {
			return nil, err
		}
		out[f.Rel] = f
	}
	return out, rows.Err()
}

func (s *Store) addFile(itemID int64, rel string, size int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO files (item_id, rel, size) VALUES (?, ?, ?)`, itemID, rel, size)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) updateFileSize(id, size int64) error {
	_, err := s.db.Exec(`UPDATE files SET size = ?, duration = 0 WHERE id = ?`, size, id)
	return err
}

func (s *Store) setDuration(id int64, d float64) error {
	_, err := s.db.Exec(`UPDATE files SET duration = ? WHERE id = ?`, d, id)
	return err
}

func (s *Store) deleteFile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

func (s *Store) FileByID(id int64) (*File, error) {
	f := &File{}
	err := s.db.QueryRow(`SELECT id, item_id, rel, size, duration, watched FROM files WHERE id = ?`, id).
		Scan(&f.ID, &f.ItemID, &f.Rel, &f.Size, &f.Duration, &f.Watched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// SetWatched marca arquivos como vistos (ou não).
func (s *Store) SetWatched(fileIDs []int64, watched bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range fileIDs {
		if _, err := tx.Exec(`UPDATE files SET watched = ? WHERE id = ?`, watched, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func notFoundIfNone(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
