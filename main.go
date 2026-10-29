package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

//go:embed web
var webFS embed.FS

func main() {
	dataDir := env("FILMES_DATA", "./data")
	mediaDir := env("FILMES_PASTA", "./filmes")
	addr := env("FILMES_ADDR", ":8080")

	for _, d := range []string{dataDir, mediaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(dataDir, "filmes.db"))
	if err != nil {
		log.Fatal(err)
	}
	qbit := NewQBit(os.Getenv("QBIT_URL"), os.Getenv("QBIT_USER"), os.Getenv("QBIT_PASS"),
		env("QBIT_PASTA", mediaDir), env("QBIT_CATEGORIA", "filmes"))
	lib, err := NewLibrary(store, mediaDir, dataDir, qbit)
	if err != nil {
		log.Fatal(err)
	}

	if qbit.Enabled() {
		if v, err := qbit.Version(); err != nil {
			log.Printf("qBittorrent: %v", err)
		} else {
			log.Printf("qBittorrent %s em %s, salvando em %s", v, qbit.base, qbit.savePath)
		}
	} else {
		log.Print("qBittorrent não configurado (QBIT_URL): a aba Baixar fica desligada")
	}
	lib.Start()
	log.Printf("filmes em %s, dados em %s, ouvindo em %s", mediaDir, dataDir, addr)
	srv := &http.Server{Addr: addr, Handler: routes(lib), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var torrentHash = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func routes(lib *Library) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	newPlayer(lib).routes(mux)

	// ---------- Acervo ----------

	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		items, err := lib.Items()
		if err != nil {
			httpError(w, err)
			return
		}
		// A lista não leva os episódios, só o resumo.
		out := make([]Item, len(items))
		for i, it := range items {
			out[i] = *it
			out[i].Files = nil
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := itemFromPath(lib, w, r)
		if ok {
			writeJSON(w, http.StatusOK, it)
		}
	})

	mux.HandleFunc("PATCH /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		it, ok := itemFromPath(lib, w, r)
		if !ok || !readJSON(w, r, &body) {
			return
		}
		title := strings.TrimSpace(body.Title)
		if utf8.RuneCountInString(title) > 200 {
			http.Error(w, "o título pode ter no máximo 200 caracteres", http.StatusBadRequest)
			return
		}
		// Título vazio volta para o tirado do nome do arquivo.
		err := lib.store.SetTitle(it.ID, title)
		if err == nil {
			err = lib.Scan()
		}
		writeResult(w, err, http.StatusNoContent, nil)
	})

	mux.HandleFunc("DELETE /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		it, ok := itemFromPath(lib, w, r)
		if ok {
			writeResult(w, lib.Delete(it), http.StatusNoContent, nil)
		}
	})

	mux.HandleFunc("POST /api/items/{id}/thumb", func(w http.ResponseWriter, r *http.Request) {
		it, ok := itemFromPath(lib, w, r)
		if ok {
			writeResult(w, lib.NewThumb(it), http.StatusNoContent, nil)
		}
	})

	// Corpo: {"fileIds": [1, 2], "watched": true}.
	mux.HandleFunc("PUT /api/watched", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FileIDs []int64 `json:"fileIds"`
			Watched bool    `json:"watched"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		err := lib.store.SetWatched(body.FileIDs, body.Watched)
		if err == nil {
			err = lib.Scan()
		}
		writeResult(w, err, http.StatusNoContent, nil)
	})

	mux.HandleFunc("POST /api/rescan", func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, lib.Scan(), http.StatusNoContent, nil)
	})

	mux.HandleFunc("GET /thumb/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, no-cache")
		http.ServeFile(w, r, lib.thumbPath(id))
	})

	// O nome do arquivo no fim da URL é só para o VLC mostrar um título bonito.
	mux.HandleFunc("GET /stream/{id}/{name...}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f, err := lib.store.FileByID(id)
		if err != nil {
			writeResult(w, err, 0, nil)
			return
		}
		p, err := lib.FilePath(f)
		if err != nil {
			httpError(w, err)
			return
		}
		if ct := videoTypes[strings.ToLower(filepath.Ext(p))]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", safeName(filepath.Base(p))))
		http.ServeFile(w, r, p) // atende Range, então dá para avançar e voltar no VLC
	})

	// Playlist .m3u para o VLC. Num filme, tem o arquivo; numa série, todos os
	// episódios a partir de ?from=<id do arquivo> (ou do primeiro não visto).
	mux.HandleFunc("GET /playlist/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimSuffix(r.PathValue("name"), ".m3u"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		it, err := lib.Item(id)
		if err != nil {
			writeResult(w, err, 0, nil)
			return
		}
		files := it.Files
		start := -1
		if from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil {
			for i, f := range files {
				if f.ID == from {
					start = i
				}
			}
		}
		if start < 0 {
			start = 0
			for i, f := range files {
				if !f.Watched {
					start = i
					break
				}
			}
		}
		base := "http://" + r.Host
		var b strings.Builder
		b.WriteString("#EXTM3U\n")
		for _, f := range files[start:] {
			title := it.Title
			if it.Series {
				title = fmt.Sprintf("%s · %s", it.Title, episodeTitle(f))
			}
			secs := -1
			if f.Duration > 0 {
				secs = int(f.Duration)
			}
			fmt.Fprintf(&b, "#EXTINF:%d,%s\n%s\n", secs, strings.ReplaceAll(title, "\n", " "), streamURL(base, f))
		}
		w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName(it.Title)+".m3u"))
		io.WriteString(w, b.String())
	})

	// ---------- Downloads (qBittorrent) ----------

	mux.HandleFunc("GET /api/downloads", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"configured": lib.qbit.Enabled(),
			"savePath":   lib.qbit.savePath,
			"free":       freeSpace(lib.mediaDir),
			"torrents":   []any{},
		}
		if !lib.qbit.Enabled() {
			writeJSON(w, http.StatusOK, out)
			return
		}
		ts, err := lib.qbit.Torrents()
		if err != nil {
			out["error"] = err.Error()
			writeJSON(w, http.StatusOK, out)
			return
		}
		items, _ := lib.Items()
		byName := map[string]int64{}
		for _, it := range items {
			byName[it.Name] = it.ID
		}
		type row struct {
			Torrent
			ItemID int64 `json:"itemId"` // 0 enquanto não está no acervo
		}
		rows := make([]row, len(ts))
		rescan := false
		for i, t := range ts {
			rows[i] = row{Torrent: t, ItemID: byName[lib.qbit.ItemName(t)]}
			if t.Progress >= 1 && rows[i].ItemID == 0 {
				rescan = true
			}
		}
		if rescan {
			// Acabou de terminar: entra no acervo já na próxima atualização.
			go lib.Scan()
		}
		out["torrents"] = rows
		writeJSON(w, http.StatusOK, out)
	})

	// Formulário com "link" (magnet ou http) ou "file" (um .torrent).
	mux.HandleFunc("POST /api/downloads", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, "envio inválido", http.StatusBadRequest)
			return
		}
		var link, fileName string
		var data []byte
		if f, h, err := r.FormFile("file"); err == nil {
			data, err = io.ReadAll(io.LimitReader(f, 4<<20))
			f.Close()
			if err != nil {
				http.Error(w, "envio interrompido", http.StatusBadRequest)
				return
			}
			fileName = h.Filename
		} else {
			link, err = NormalizeLink(r.FormValue("link"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		err := lib.qbit.Add(link, data, fileName)
		var exists ErrExists
		switch {
		case errors.As(err, &exists):
			http.Error(w, exists.Error(), http.StatusConflict)
		case errors.Is(err, ErrRefused):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrBadTorrent):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			log.Printf("adicionando torrent: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	})

	// Vídeos enviados pelo navegador, lidos aos pedaços para não ocupar memória.
	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "envio inválido", http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "envio interrompido", http.StatusBadRequest)
				return
			}
			if part.FormName() != "file" || part.FileName() == "" {
				continue
			}
			err = lib.Upload(part.FileName(), part)
			switch {
			case errors.Is(err, ErrBadUpload):
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			case errors.Is(err, ErrExistsUpload):
				http.Error(w, err.Error(), http.StatusConflict)
				return
			case err != nil:
				log.Printf("recebendo %s: %v", part.FileName(), err)
				http.Error(w, "não deu para gravar o vídeo no servidor", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
	})

	torrentAction := func(do func(hash string, r *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hash := r.PathValue("hash")
			if !torrentHash.MatchString(hash) {
				http.NotFound(w, r)
				return
			}
			if err := do(hash, r); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /api/downloads/{hash}/pause", torrentAction(func(h string, r *http.Request) error { return lib.qbit.Pause(h) }))
	mux.HandleFunc("POST /api/downloads/{hash}/resume", torrentAction(func(h string, r *http.Request) error { return lib.qbit.Resume(h) }))
	// ?files=1 apaga também o que já foi baixado.
	mux.HandleFunc("DELETE /api/downloads/{hash}", torrentAction(func(h string, r *http.Request) error {
		err := lib.qbit.Delete(h, r.URL.Query().Get("files") == "1")
		if err == nil {
			time.AfterFunc(3*time.Second, func() { lib.Scan() })
		}
		return err
	}))

	return mux
}

var videoTypes = map[string]string{
	".mkv": "video/x-matroska", ".mp4": "video/mp4", ".m4v": "video/mp4", ".avi": "video/x-msvideo",
	".mov": "video/quicktime", ".webm": "video/webm", ".ts": "video/mp2t", ".m2ts": "video/mp2t",
	".wmv": "video/x-ms-wmv", ".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".flv": "video/x-flv", ".ogv": "video/ogg",
}

func streamURL(base string, f *File) string {
	return fmt.Sprintf("%s/stream/%d/%s", base, f.ID, url.PathEscape(filepath.Base(f.Rel)))
}

func episodeTitle(f *File) string {
	if f.Episode > 0 {
		return fmt.Sprintf("T%d E%d", f.Season, f.Episode)
	}
	return f.Label
}

// freeSpace devolve os bytes livres no disco da pasta de filmes (0 se não souber).
func freeSpace(dir string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, s)
}

func itemFromPath(lib *Library, w http.ResponseWriter, r *http.Request) (*Item, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	it, err := lib.Item(id)
	if err != nil {
		writeResult(w, err, 0, nil)
		return nil, false
	}
	return it, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return false
	}
	return true
}

// writeResult responde com o erro adequado ou, se err for nil, com status e v.
func writeResult(w http.ResponseWriter, err error, status int, v any) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "não encontrado", http.StatusNotFound)
	case err != nil:
		httpError(w, err)
	case v == nil:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, v)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "erro interno", http.StatusInternalServerError)
}
