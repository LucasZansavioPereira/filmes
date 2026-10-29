package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Library acompanha a pasta de filmes. Cada entrada do primeiro nível (um
// arquivo de vídeo ou uma pasta com vídeos dentro) vira um item.
type Library struct {
	store    *Store
	mediaDir string
	dataDir  string
	qbit     *QBit

	mu       sync.Mutex // protege items e lastScan; scanMu serializa as varreduras
	scanMu   sync.Mutex
	items    []*Item
	lastScan time.Time
	work     chan struct{}
}

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".webm": true,
	".ts": true, ".m2ts": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".flv": true, ".ogv": true,
}

func isVideo(name string) bool { return videoExts[strings.ToLower(filepath.Ext(name))] }

func NewLibrary(store *Store, mediaDir, dataDir string, qbit *QBit) (*Library, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "thumbs"), 0o755); err != nil {
		return nil, err
	}
	return &Library{store: store, mediaDir: mediaDir, dataDir: dataDir, qbit: qbit, work: make(chan struct{}, 1)}, nil
}

func (l *Library) thumbPath(id int64) string {
	return filepath.Join(l.dataDir, "thumbs", strconv.FormatInt(id, 10)+".jpg")
}

// Start varre a pasta agora e depois a cada minuto, e roda o trabalhador que
// tira capas e lê a duração dos vídeos.
func (l *Library) Start() {
	if err := l.Scan(); err != nil {
		log.Printf("varredura: %v", err)
	}
	go func() {
		for range time.Tick(time.Minute) {
			if err := l.Scan(); err != nil {
				log.Printf("varredura: %v", err)
			}
		}
	}()
	go l.worker()
}

// Items devolve o acervo, varrendo a pasta de novo se a última varredura tem
// mais de alguns segundos.
func (l *Library) Items() ([]*Item, error) {
	l.mu.Lock()
	stale := time.Since(l.lastScan) > 10*time.Second
	l.mu.Unlock()
	if stale {
		if err := l.Scan(); err != nil {
			return nil, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.items, nil
}

func (l *Library) Item(id int64) (*Item, error) {
	items, err := l.Items()
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.ID == id {
			return it, nil
		}
	}
	return nil, ErrNotFound
}

// Scan sincroniza o banco com o que está na pasta.
func (l *Library) Scan() error {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()

	entries, err := os.ReadDir(l.mediaDir)
	if err != nil {
		return fmt.Errorf("lendo a pasta de filmes %s: %w", l.mediaDir, err)
	}
	rows, err := l.store.itemRows()
	if err != nil {
		return err
	}
	known, err := l.store.fileRows()
	if err != nil {
		return err
	}

	// O que o qBittorrent ainda está baixando só entra no acervo quando terminar.
	downloading := l.qbit.Downloading()

	seenItems := map[string]bool{}
	seenFiles := map[string]bool{}
	var items []*Item
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || downloading[name] {
			continue
		}
		vids := l.videosIn(name, e.IsDir())
		if len(vids) == 0 {
			continue
		}
		row := rows[name]
		if row == nil {
			// Item novo. A data de entrada é a do arquivo, para que um acervo
			// antigo apareça na ordem em que foi baixado.
			added := now()
			if info, err := e.Info(); err == nil {
				added = info.ModTime().UTC().Format(time.RFC3339)
			}
			id, err := l.store.addItem(name, added)
			if err != nil {
				return err
			}
			row = &itemRow{ID: id, Name: name, AddedAt: added}
			log.Printf("novo no acervo: %s", name)
		}
		seenItems[name] = true

		it := &Item{ID: row.ID, Name: name, AddedAt: row.AddedAt, HasThumb: row.HasThumb}
		title, year, series := parseName(name, e.IsDir())
		it.Title, it.Year, it.Series = title, year, series
		if row.Title != "" {
			it.Title, it.Year = row.Title, ""
		}
		for _, v := range vids {
			seenFiles[v.rel] = true
			f := known[v.rel]
			switch {
			case f == nil:
				id, err := l.store.addFile(row.ID, v.rel, v.size)
				if err != nil {
					return err
				}
				f = &File{ID: id, ItemID: row.ID, Rel: v.rel, Size: v.size}
			case f.Size != v.size:
				// Arquivo trocado (ou ainda crescendo): a duração precisa ser lida de novo.
				if err := l.store.updateFileSize(f.ID, v.size); err != nil {
					return err
				}
				f.Size, f.Duration = v.size, 0
			}
			nf := *f
			nf.Season, nf.Episode = parseEpisode(filepath.Base(v.rel))
			nf.Label = fileLabel(v.rel, nf.Season, nf.Episode)
			if nf.Episode > 0 {
				it.Series = true
			}
			it.Files = append(it.Files, &nf)
			it.Count++
			it.Size += nf.Size
			if nf.Duration > 0 {
				it.Duration += nf.Duration
			}
			if nf.Watched {
				it.Watched++
			}
		}
		if it.Count > 1 {
			it.Series = true
		}
		sortFiles(it.Files)
		items = append(items, it)
	}

	for rel, f := range known {
		if !seenFiles[rel] {
			if err := l.store.deleteFile(f.ID); err != nil {
				return err
			}
		}
	}
	for name, row := range rows {
		if !seenItems[name] {
			log.Printf("saiu do acervo: %s", name)
			if err := l.store.deleteItem(row.ID); err != nil {
				return err
			}
			os.Remove(l.thumbPath(row.ID))
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].AddedAt > items[j].AddedAt })
	l.mu.Lock()
	l.items = items
	l.lastScan = time.Now()
	l.mu.Unlock()
	l.kick()
	return nil
}

type video struct {
	rel  string
	size int64
}

// videosIn lista os vídeos de uma entrada do primeiro nível. Amostras
// ("sample") que vêm em muitos torrents ficam de fora.
func (l *Library) videosIn(name string, dir bool) []video {
	if !dir {
		info, err := os.Stat(filepath.Join(l.mediaDir, name))
		if err != nil || !isVideo(name) || !info.Mode().IsRegular() {
			return nil
		}
		return []video{{name, info.Size()}}
	}
	var out []video
	root := filepath.Join(l.mediaDir, name)
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || strings.EqualFold(d.Name(), "sample")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isVideo(d.Name()) || samplePattern.MatchString(d.Name()) || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(l.mediaDir, path)
		out = append(out, video{filepath.ToSlash(rel), info.Size()})
		return nil
	})
	return out
}

var samplePattern = regexp.MustCompile(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)`)

// entryPath devolve o caminho no disco de um item, conferindo que ele está
// mesmo dentro da pasta de filmes.
func (l *Library) entryPath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("nome inválido: %q", name)
	}
	return filepath.Join(l.mediaDir, name), nil
}

func (l *Library) FilePath(f *File) (string, error) {
	p := filepath.Join(l.mediaDir, filepath.FromSlash(f.Rel))
	rel, err := filepath.Rel(l.mediaDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("caminho fora da pasta de filmes: %q", f.Rel)
	}
	return p, nil
}

// Delete apaga um item do disco. Se ele veio de um torrent que ainda está no
// qBittorrent, o torrent é removido junto (senão o qBittorrent baixaria tudo de novo).
func (l *Library) Delete(it *Item) error {
	path, err := l.entryPath(it.Name)
	if err != nil {
		return err
	}
	if l.qbit.Enabled() {
		if ts, err := l.qbit.Torrents(); err == nil {
			for _, t := range ts {
				if l.qbit.ItemName(t) == it.Name {
					if err := l.qbit.Delete(t.Hash, true); err != nil {
						log.Printf("removendo torrent de %s: %v", it.Name, err)
					}
				}
			}
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return l.Scan()
}

// Upload grava um vídeo enviado pelo navegador na pasta de filmes. Ele vai
// primeiro para um arquivo oculto (a varredura ignora nomes com ponto) e só
// ganha o nome final quando chega inteiro.
func (l *Library) Upload(name string, src io.Reader) error {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if strings.HasPrefix(name, ".") || !isVideo(name) {
		return fmt.Errorf("%w: %q não é um vídeo", ErrBadUpload, name)
	}
	dst, err := l.entryPath(name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadUpload, err)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%w: já existe um arquivo chamado %q", ErrExistsUpload, name)
	}
	tmp, err := os.CreateTemp(l.mediaDir, ".envio-*.part")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		if _, serr := os.Stat(dst); serr == nil {
			err = fmt.Errorf("%w: já existe um arquivo chamado %q", ErrExistsUpload, name)
		} else {
			err = os.Rename(tmp.Name(), dst)
		}
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return l.Scan()
}

var (
	ErrBadUpload    = errors.New("envio inválido")
	ErrExistsUpload = errors.New("arquivo repetido")
)

// ---------- Capas e duração ----------

func (l *Library) kick() {
	select {
	case l.work <- struct{}{}:
	default:
	}
}

// worker roda um ffmpeg/ffprobe por vez, para não pesar no servidor.
func (l *Library) worker() {
	for range l.work {
		l.mu.Lock()
		items := l.items
		l.mu.Unlock()
		rows, err := l.store.itemRows()
		if err != nil {
			log.Printf("capas: %v", err)
			continue
		}
		changed := false
		for _, it := range items {
			for _, f := range it.Files {
				if f.Duration != 0 {
					continue
				}
				d := probeDuration(l, f)
				if d <= 0 {
					d = -1 // não tenta de novo até o arquivo mudar
				}
				l.store.setDuration(f.ID, d)
				changed = true
			}
			if r := rows[it.Name]; r != nil && !r.ThumbTried {
				l.makeThumb(it, 0.15)
				changed = true
			}
		}
		if changed {
			if err := l.Scan(); err != nil {
				log.Printf("varredura: %v", err)
			}
		}
	}
}

// NewThumb tira outra capa, de um ponto aleatório do vídeo.
func (l *Library) NewThumb(it *Item) error {
	if !l.makeThumb(it, 0.1+rand.Float64()*0.7) {
		return fmt.Errorf("não consegui tirar um quadro deste vídeo")
	}
	return l.Scan()
}

// makeThumb tira um quadro do primeiro vídeo do item na posição at (fração da duração).
func (l *Library) makeThumb(it *Item, at float64) bool {
	if len(it.Files) == 0 {
		return false
	}
	f := it.Files[0]
	src, err := l.FilePath(f)
	if err != nil {
		return false
	}
	secs := 60.0
	if f.Duration > 0 {
		secs = f.Duration * at
	}
	dst := l.thumbPath(it.ID)
	ok := frame(src, dst, secs) || frame(src, dst, 0)
	l.store.setThumb(it.ID, ok)
	return ok
}

func frame(src, dst string, secs float64) bool {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return false
	}
	tmp := dst + ".tmp.jpg"
	defer os.Remove(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(secs, 'f', 1, 64), "-i", src,
		"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "4", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg em %s: %v %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
		return false
	}
	if info, err := os.Stat(tmp); err != nil || info.Size() == 0 {
		return false
	}
	return os.Rename(tmp, dst) == nil
}

func probeDuration(l *Library, f *File) float64 {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	src, err := l.FilePath(f)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", src).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

// ---------- Nomes ----------

var (
	episodePattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[ ._-]?e(\d{1,3})|(?:^|[^a-z0-9])(\d{1,2})x(\d{2,3})(?:[^a-z0-9]|$)`)
	seasonPattern  = regexp.MustCompile(`(?i)(?:^|[ ._\-\[(])(?:s\d{1,2}(?:[ ._-]?e\d{1,3})?|season[ ._]?\d+|temporada[ ._]?\d+|\d{1,2}[ ._]?temporada|complete[ ._]series|s[ée]rie[ ._]completa)(?:[ ._\-\])]|$)`)
	yearPattern    = regexp.MustCompile(`(?:^|[ ._\-\[(])((?:19|20)\d{2})`)
	junkPattern    = regexp.MustCompile(`(?i)(?:^|[ ._\-\[(])(?:2160p|1080p|720p|480p|4k|uhd|hdr|bluray|blu-ray|brrip|bdrip|web-?dl|webrip|web|hdtv|dvdrip|x264|x265|h\.?264|h\.?265|hevc|10bit|dual|dublado|legendado|nacional|multi|remux|proper|repack|extended|unrated|aac|ac3|dts|ddp?5\.1)(?:[ ._\-\])]|$)`)
	prefixJunk     = regexp.MustCompile(`(?i)^(?:\[[^\]]*\]\s*|www\.[^ ]+\s*-?\s*)+`)
	spaces         = regexp.MustCompile(`\s+`)
)

// parseName tira um título legível do nome de um torrent, por exemplo
// "The.Matrix.1999.1080p.BluRay.x264-GRP" vira "The Matrix" (1999).
func parseName(name string, dir bool) (title, year string, series bool) {
	s := name
	if !dir {
		s = strings.TrimSuffix(s, filepath.Ext(s))
	}
	s = prefixJunk.ReplaceAllString(s, "")
	cut := len(s)
	if loc := seasonPattern.FindStringIndex(s); loc != nil {
		series = true
		cut = min(cut, loc[0])
	}
	if loc := episodePattern.FindStringIndex(s); loc != nil {
		series = true
		cut = min(cut, loc[0])
	}
	yearAt := 0
	// O último ano é o do lançamento ("2001 A Space Odyssey 1968"). O RE2 não
	// tem lookahead, então o separador depois do ano é conferido aqui.
	for _, m := range yearPattern.FindAllStringSubmatchIndex(s, -1) {
		if m[3] < len(s) && !strings.ContainsRune(" ._-])", rune(s[m[3]])) {
			continue
		}
		if m[2] > 0 { // um ano no começo é parte do título ("1917")
			year, yearAt = s[m[2]:m[3]], m[0]
		}
	}
	if year != "" {
		cut = min(cut, yearAt)
	}
	if loc := junkPattern.FindStringIndex(s); loc != nil {
		cut = min(cut, loc[0])
	}
	if cut > 0 {
		s = s[:cut]
	}
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " -([")
	if s == "" {
		s = name
	}
	return s, year, series
}

func parseEpisode(base string) (season, episode int) {
	m := episodePattern.FindStringSubmatch(base)
	if m == nil {
		return 0, 0
	}
	if m[1] != "" {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
	} else {
		season, _ = strconv.Atoi(m[3])
		episode, _ = strconv.Atoi(m[4])
	}
	return season, episode
}

func fileLabel(rel string, season, episode int) string {
	if episode > 0 {
		return fmt.Sprintf("Episódio %d", episode)
	}
	base := filepath.Base(rel)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func sortFiles(files []*File) {
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.Season != b.Season {
			return a.Season < b.Season
		}
		if a.Episode != b.Episode {
			return a.Episode < b.Episode
		}
		return naturalLess(a.Rel, b.Rel)
	})
}

var digits = regexp.MustCompile(`\d+|\D+`)

// naturalLess ordena "ep 2" antes de "ep 10".
func naturalLess(a, b string) bool {
	pa, pb := digits.FindAllString(strings.ToLower(a), -1), digits.FindAllString(strings.ToLower(b), -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil && na != nb {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}
