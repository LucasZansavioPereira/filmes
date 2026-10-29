package main

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// QBit fala com a Web API do qBittorrent (v2, qBittorrent 4.1 em diante).
type QBit struct {
	base     string // http://host:porta, sem barra no fim
	user     string
	pass     string
	savePath string // pasta de filmes como o qBittorrent enxerga
	category string
	hc       *http.Client

	mu          sync.Mutex
	categoryOK  bool
	lastErr     error
	lastErrTime time.Time
	// Última falha de login (ErrQbitLogin), para não insistir.
	loginErr     error
	loginErrTime time.Time
}

// Torrent é o que a app usa de /api/v2/torrents/info.
type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Progress    float64 `json:"progress"`
	DLSpeed     int64   `json:"dlspeed"`
	UPSpeed     int64   `json:"upspeed"`
	ETA         int64   `json:"eta"`
	State       string  `json:"state"`
	Size        int64   `json:"size"`
	Downloaded  int64   `json:"downloaded"`
	NumSeeds    int     `json:"num_seeds"`
	NumLeechs   int     `json:"num_leechs"`
	AddedOn     int64   `json:"added_on"`
	ContentPath string  `json:"content_path"`
	SavePath    string  `json:"save_path"`
}

var (
	ErrNoQbit     = errors.New("o qBittorrent não está configurado (QBIT_URL)")
	ErrQbitLogin  = errors.New("o qBittorrent recusou o usuário ou a senha (QBIT_USER e QBIT_PASS)")
	ErrBadTorrent = errors.New("o qBittorrent não aceitou este torrent (link ou arquivo inválido)")
	ErrRefused    = errors.New("o qBittorrent recusou o torrent (talvez ele já esteja na lista, ou o link é inválido)")
)

// ErrExists é devolvido quando o torrent já está no qBittorrent.
type ErrExists struct{ Name string }

func (e ErrExists) Error() string {
	return fmt.Sprintf("este torrent já está no qBittorrent (%s)", e.Name)
}

func NewQBit(base, user, pass, savePath, category string) *QBit {
	jar, _ := cookiejar.New(nil)
	return &QBit{
		base:     strings.TrimRight(base, "/"),
		user:     user,
		pass:     pass,
		savePath: strings.TrimRight(savePath, "/"),
		category: category,
		hc:       &http.Client{Jar: jar, Timeout: 20 * time.Second},
	}
}

func (q *QBit) Enabled() bool { return q != nil && q.base != "" }

// req monta uma requisição com o Referer que o qBittorrent exige para não
// achar que é um ataque CSRF.
func (q *QBit) req(method, p string, body io.Reader, contentType string) (*http.Request, error) {
	r, err := http.NewRequest(method, q.base+p, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Referer", q.base+"/")
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r, nil
}

// login entra no qBittorrent. Depois de uma senha errada, espera um minuto
// antes de tentar de novo: o qBittorrent bane o IP após algumas falhas
// seguidas, e a aba Baixar pergunta a cada 2 segundos.
func (q *QBit) login() error {
	q.mu.Lock()
	failed := q.loginErr != nil && time.Since(q.loginErrTime) < time.Minute
	q.mu.Unlock()
	if failed {
		return q.loginErr
	}
	err := q.doLogin()
	if err != nil && !errors.Is(err, ErrQbitLogin) {
		return err // qBittorrent fora do ar: pode tentar de novo já
	}
	q.mu.Lock()
	q.loginErr, q.loginErrTime = err, time.Now()
	q.mu.Unlock()
	return err
}

func (q *QBit) doLogin() error {
	form := url.Values{"username": {q.user}, "password": {q.pass}}
	r, err := q.req("POST", "/api/v2/auth/login", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	res, err := q.hc.Do(r)
	if err != nil {
		return fmt.Errorf("não consegui falar com o qBittorrent em %s: %w", q.base, err)
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusForbidden:
		return errors.New("o qBittorrent bloqueou o login por excesso de tentativas; espere um pouco")
	case res.StatusCode != http.StatusOK, strings.TrimSpace(string(b)) == "Fails.":
		return ErrQbitLogin
	}
	return nil
}

// call faz uma chamada e, se a sessão expirou (403), entra de novo e repete.
// build é chamada a cada tentativa porque o corpo não pode ser reaproveitado.
func (q *QBit) call(build func() (*http.Request, error)) (int, []byte, error) {
	if !q.Enabled() {
		return 0, nil, ErrNoQbit
	}
	for try := 0; ; try++ {
		r, err := build()
		if err != nil {
			return 0, nil, err
		}
		res, err := q.hc.Do(r)
		if err != nil {
			return 0, nil, q.remember(fmt.Errorf("não consegui falar com o qBittorrent em %s: %w", q.base, err))
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if res.StatusCode == http.StatusForbidden && try == 0 {
			if err := q.login(); err != nil {
				return 0, nil, q.remember(err)
			}
			continue
		}
		q.remember(nil)
		return res.StatusCode, b, nil
	}
}

func (q *QBit) remember(err error) error {
	q.mu.Lock()
	q.lastErr, q.lastErrTime = err, time.Now()
	q.mu.Unlock()
	return err
}

func (q *QBit) get(p string) (int, []byte, error) {
	return q.call(func() (*http.Request, error) { return q.req("GET", p, nil, "") })
}

func (q *QBit) post(p string, form url.Values) (int, []byte, error) {
	return q.call(func() (*http.Request, error) {
		return q.req("POST", p, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	})
}

// Version confere a conexão e devolve a versão do qBittorrent.
func (q *QBit) Version() (string, error) {
	st, b, err := q.get("/api/v2/app/version")
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", fmt.Errorf("o qBittorrent respondeu %d", st)
	}
	return strings.TrimSpace(string(b)), nil
}

func (q *QBit) torrents(query url.Values) ([]Torrent, error) {
	st, b, err := q.get("/api/v2/torrents/info?" + query.Encode())
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("o qBittorrent respondeu %d", st)
	}
	var out []Torrent
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("resposta estranha do qBittorrent: %w", err)
	}
	return out, nil
}

// Torrents lista os torrents da categoria da app, mais novos primeiro.
func (q *QBit) Torrents() ([]Torrent, error) {
	return q.torrents(url.Values{"category": {q.category}, "sort": {"added_on"}, "reverse": {"true"}})
}

// Downloading devolve os itens da pasta de filmes que ainda estão baixando.
// Se o qBittorrent não responder, devolve vazio (e tudo aparece no acervo).
func (q *QBit) Downloading() map[string]bool {
	out := map[string]bool{}
	if !q.Enabled() {
		return out
	}
	q.mu.Lock()
	recentErr := q.lastErr != nil && time.Since(q.lastErrTime) < 30*time.Second
	q.mu.Unlock()
	if recentErr {
		return out // não trava cada varredura esperando um qBittorrent fora do ar
	}
	ts, err := q.Torrents()
	if err != nil {
		return out
	}
	for _, t := range ts {
		if t.Progress < 1 {
			out[q.ItemName(t)] = true
		}
	}
	return out
}

// ItemName é o nome, na pasta de filmes, da entrada que o torrent cria.
func (q *QBit) ItemName(t Torrent) string {
	cp := strings.TrimRight(t.ContentPath, "/")
	for _, base := range []string{q.savePath, strings.TrimRight(t.SavePath, "/")} {
		if base != "" && strings.HasPrefix(cp, base+"/") {
			rest := strings.TrimPrefix(cp, base+"/")
			return strings.SplitN(rest, "/", 2)[0]
		}
	}
	if cp != "" {
		return path.Base(cp)
	}
	return t.Name
}

func (q *QBit) ensureCategory() {
	q.mu.Lock()
	ok := q.categoryOK
	q.mu.Unlock()
	if ok {
		return
	}
	// 409 quer dizer que a categoria já existe; serve do mesmo jeito.
	st, _, err := q.post("/api/v2/torrents/createCategory", url.Values{"category": {q.category}, "savePath": {q.savePath}})
	if err == nil && (st == http.StatusOK || st == http.StatusConflict) {
		q.mu.Lock()
		q.categoryOK = true
		q.mu.Unlock()
	}
}

var (
	btihPattern = regexp.MustCompile(`(?i)xt=urn:btih:([a-z0-9]+)`)
	hashPattern = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)
)

// infoHash tira o hash (hex, minúsculo) de um link magnet, se der.
func infoHash(magnet string) string {
	m := btihPattern.FindStringSubmatch(magnet)
	if m == nil {
		return ""
	}
	h := m[1]
	if len(h) == 32 { // base32
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
		if err != nil {
			return ""
		}
		return hex.EncodeToString(b)
	}
	return strings.ToLower(h)
}

// NormalizeLink aceita um link magnet, um link http(s) para um .torrent ou só
// o hash de 40 caracteres.
func NormalizeLink(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case hashPattern.MatchString(s):
		return "magnet:?xt=urn:btih:" + strings.ToLower(s), nil
	case strings.HasPrefix(strings.ToLower(s), "magnet:?"):
		if infoHash(s) == "" {
			return "", errors.New("este link magnet não tem o hash do torrent (xt=urn:btih:...)")
		}
		return s, nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		return s, nil
	}
	return "", errors.New("cole um link magnet (magnet:?...) ou o endereço de um arquivo .torrent")
}

// Add manda o qBittorrent baixar um link ou um arquivo .torrent para a pasta de filmes.
func (q *QBit) Add(link string, file []byte, fileName string) error {
	if !q.Enabled() {
		return ErrNoQbit
	}
	if h := infoHash(link); h != "" {
		ts, err := q.torrents(url.Values{"hashes": {h}})
		if err != nil {
			return err
		}
		if len(ts) > 0 {
			return ErrExists{ts[0].Name}
		}
	}
	q.ensureCategory()
	st, b, err := q.call(func() (*http.Request, error) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if link != "" {
			mw.WriteField("urls", link)
		}
		if file != nil {
			fw, err := mw.CreateFormFile("torrents", fileName)
			if err != nil {
				return nil, err
			}
			fw.Write(file)
		}
		mw.WriteField("category", q.category)
		if q.savePath != "" {
			mw.WriteField("savepath", q.savePath)
			mw.WriteField("autoTMM", "false")
		}
		mw.Close()
		return q.req("POST", "/api/v2/torrents/add", &buf, mw.FormDataContentType())
	})
	if err != nil {
		return err
	}
	body := strings.TrimSpace(string(b))
	switch {
	case st == http.StatusUnsupportedMediaType, st == http.StatusBadRequest:
		return ErrBadTorrent
	case st == http.StatusConflict:
		return ErrExists{"mesmo hash"}
	case st != http.StatusOK:
		return fmt.Errorf("o qBittorrent respondeu %d: %s", st, body)
	case body == "Fails.":
		// Versões antigas respondem só isso, inclusive para torrent repetido.
		return ErrRefused
	}
	// qBittorrent 5.1+ responde em JSON com contagens.
	var res struct {
		Success *int `json:"success_count"`
		Failure int  `json:"failure_count"`
		Pending int  `json:"pending_count"`
	}
	if json.Unmarshal(b, &res) == nil && res.Success != nil && *res.Success == 0 && res.Pending == 0 && res.Failure > 0 {
		return ErrRefused
	}
	return nil
}

// action chama a rota nova e, se o qBittorrent for antigo (404), a antiga.
// O qBittorrent 5 trocou pause/resume por stop/start.
func (q *QBit) action(newPath, oldPath, hash string) error {
	form := url.Values{"hashes": {hash}}
	st, _, err := q.post(newPath, form)
	if err == nil && st == http.StatusNotFound && oldPath != "" {
		st, _, err = q.post(oldPath, form)
	}
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return fmt.Errorf("o qBittorrent respondeu %d", st)
	}
	return nil
}

func (q *QBit) Pause(hash string) error {
	return q.action("/api/v2/torrents/stop", "/api/v2/torrents/pause", hash)
}

func (q *QBit) Resume(hash string) error {
	return q.action("/api/v2/torrents/start", "/api/v2/torrents/resume", hash)
}

func (q *QBit) Delete(hash string, deleteFiles bool) error {
	st, _, err := q.post("/api/v2/torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {fmt.Sprint(deleteFiles)}})
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return fmt.Errorf("o qBittorrent respondeu %d", st)
	}
	return nil
}
