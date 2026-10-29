package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Tocar no navegador ----------
//
// O navegador toca vídeo H.264, VP9 ou AV1 com áudio AAC, MP3, Opus ou FLAC, mas
// não toca os áudios de filme (AC3, E-AC3, DTS) e o Safari não abre MKV. Então:
// um mp4/webm que o navegador já toca vai direto por /stream; o resto passa pelo
// ffmpeg em /play, que copia o vídeo sem reconverter e só converte o áudio para
// AAC (leve, até no Celeron). Vídeo em outro formato (HEVC, MPEG-2, XviD) seria
// pesado demais para converter: esse fica para o VLC.

// Track é uma faixa de áudio ou legenda. Index é a posição entre as faixas do
// mesmo tipo (o N de 0:a:N e 0:s:N no ffmpeg).
type Track struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Lang     string `json:"lang"`
	Title    string `json:"title"`
	Channels int    `json:"channels,omitempty"`
	Default  bool   `json:"default"`
}

type MediaInfo struct {
	Duration float64 `json:"duration"`
	Video    string  `json:"video"`
	Mode     string  `json:"mode"` // "direct" (arquivo como está), "remux" (pelo ffmpeg) ou "vlc"
	Audio    []Track `json:"audio"`
	Subs     []Track `json:"subs"` // só legendas em texto, que dá para converter para WebVTT
}

var (
	webVideo = map[string]bool{"h264": true, "vp9": true, "av1": true}
	webAudio = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
	textSubs = map[string]bool{"subrip": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true}
)

type player struct {
	lib   *Library
	slots chan struct{} // limita quantos ffmpeg tocam ao mesmo tempo

	mu    sync.Mutex
	infos map[string]*MediaInfo // por caminho + tamanho do arquivo
	subMu sync.Mutex            // extrai as legendas de um arquivo por vez
}

func newPlayer(lib *Library) *player {
	if err := os.MkdirAll(filepath.Join(lib.dataDir, "subs"), 0o755); err != nil {
		log.Printf("legendas: %v", err)
	}
	return &player{lib: lib, slots: make(chan struct{}, 2), infos: map[string]*MediaInfo{}}
}

func (p *player) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/files/{id}/media", func(w http.ResponseWriter, r *http.Request) {
		f, src, ok := p.file(w, r)
		if !ok {
			return
		}
		info, err := p.info(f, src)
		if err == nil && len(info.Subs) > 0 {
			// Extrair as legendas leva uns segundos: já começa, para estarem
			// prontas quando forem escolhidas no player.
			go func() {
				if _, err := p.subtitle(f, src, info, 0); err != nil {
					log.Print(err)
				}
			}()
		}
		writeResult(w, err, http.StatusOK, info)
	})

	// /play/{id}?t=<segundos>&a=<faixa de áudio>: o vídeo em MP4 fragmentado, a
	// partir de t. Avançar e voltar no player pede outro t.
	mux.HandleFunc("GET /play/{id}", func(w http.ResponseWriter, r *http.Request) {
		f, src, ok := p.file(w, r)
		if !ok {
			return
		}
		info, err := p.info(f, src)
		if err != nil {
			httpError(w, err)
			return
		}
		if info.Mode == "vlc" {
			http.Error(w, "o navegador não toca este vídeo: abra no VLC", http.StatusUnprocessableEntity)
			return
		}
		t, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
		a, _ := strconv.Atoi(r.URL.Query().Get("a"))
		select {
		case p.slots <- struct{}{}:
			defer func() { <-p.slots }()
		case <-r.Context().Done():
			return
		}
		p.remux(w, r, src, info, max(t, 0), a)
	})

	// /subs/{id}/{n}.vtt?shift=<segundos>: a legenda n em WebVTT, adiantada em
	// shift segundos (o vídeo do /play começa do zero no ponto pedido).
	mux.HandleFunc("GET /subs/{id}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f, src, ok := p.file(w, r)
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("name"), ".vtt"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		info, err := p.info(f, src)
		if err != nil {
			httpError(w, err)
			return
		}
		if n < 0 || n >= len(info.Subs) {
			http.NotFound(w, r)
			return
		}
		vtt, err := p.subtitle(f, src, info, n)
		if err != nil {
			httpError(w, err)
			return
		}
		shift, _ := strconv.ParseFloat(r.URL.Query().Get("shift"), 64)
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Write(shiftVTT(vtt, shift))
	})
}

func (p *player) file(w http.ResponseWriter, r *http.Request) (*File, string, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, "", false
	}
	f, err := p.lib.store.FileByID(id)
	if err != nil {
		writeResult(w, err, 0, nil)
		return nil, "", false
	}
	src, err := p.lib.FilePath(f)
	if err != nil {
		httpError(w, err)
		return nil, "", false
	}
	return f, src, true
}

// info lê as faixas do arquivo com o ffprobe (uma vez por arquivo) e decide como tocar.
func (p *player) info(f *File, src string) (*MediaInfo, error) {
	key := fmt.Sprintf("%s|%d", src, f.Size)
	p.mu.Lock()
	info := p.infos[key]
	p.mu.Unlock()
	if info != nil {
		return info, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-of", "json",
		"-show_entries", "format=duration:stream=codec_type,codec_name,channels:stream_tags=language,title:stream_disposition=default",
		src).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe em %s: %w", filepath.Base(src), err)
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type        string            `json:"codec_type"`
			Codec       string            `json:"codec_name"`
			Channels    int               `json:"channels"`
			Tags        map[string]string `json:"tags"`
			Disposition map[string]int    `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("ffprobe em %s: %w", filepath.Base(src), err)
	}

	info = &MediaInfo{Audio: []Track{}, Subs: []Track{}}
	info.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	nAudio, nSubs := 0, 0
	for _, s := range probe.Streams {
		t := Track{Codec: s.Codec, Lang: s.Tags["language"], Title: s.Tags["title"], Channels: s.Channels, Default: s.Disposition["default"] == 1}
		switch s.Type {
		case "video":
			// A capa embutida em alguns MKV também aparece como vídeo (mjpeg, png).
			if info.Video == "" && s.Disposition["attached_pic"] == 0 {
				info.Video = s.Codec
			}
		case "audio":
			t.Index = nAudio
			nAudio++
			info.Audio = append(info.Audio, t)
		case "subtitle":
			t.Index = nSubs
			nSubs++
			if textSubs[s.Codec] {
				info.Subs = append(info.Subs, t)
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(src))
	switch {
	case !webVideo[info.Video]:
		info.Mode = "vlc"
	case (ext == ".mp4" || ext == ".m4v" || ext == ".webm") && len(info.Audio) <= 1 &&
		(len(info.Audio) == 0 || webAudio[info.Audio[0].Codec]):
		info.Mode = "direct"
	default:
		info.Mode = "remux"
	}

	p.mu.Lock()
	p.infos[key] = info
	p.mu.Unlock()
	return info, nil
}

func (p *player) remux(w http.ResponseWriter, r *http.Request, src string, info *MediaInfo, t float64, a int) {
	args := []string{"-nostdin", "-loglevel", "error"}
	if t > 0 {
		args = append(args, "-ss", strconv.FormatFloat(t, 'f', 2, 64))
	}
	args = append(args, "-i", src, "-map", "0:v:0", "-c:v", "copy")
	if a >= 0 && a < len(info.Audio) {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", a))
		if c := info.Audio[a].Codec; c == "aac" || c == "mp3" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192k")
		}
	}
	args = append(args, "-sn", "-dn", "-map_chapters", "-1", "-avoid_negative_ts", "make_zero",
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1")

	// Quando o navegador fecha a conexão (trocou de ponto, fechou a página), o ffmpeg para junto.
	cmd := exec.CommandContext(r.Context(), "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = w
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store")
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		log.Printf("ffmpeg tocando %s: %v %s", filepath.Base(src), err, strings.TrimSpace(stderr.String()))
	}
}

// subtitle devolve a legenda n em WebVTT. Na primeira vez, o ffmpeg extrai todas
// as legendas do arquivo de uma vez (ele precisa ler o arquivo inteiro, então
// demora uns segundos) e guarda em data/subs.
func (p *player) subtitle(f *File, src string, info *MediaInfo, n int) ([]byte, error) {
	path := func(i int) string {
		return filepath.Join(p.lib.dataDir, "subs", fmt.Sprintf("%d-%d-%d.vtt", f.ID, f.Size, i))
	}
	if b, err := os.ReadFile(path(n)); err == nil {
		return b, nil
	}
	p.subMu.Lock()
	defer p.subMu.Unlock()
	if b, err := os.ReadFile(path(n)); err == nil {
		return b, nil
	}

	args := []string{"-nostdin", "-loglevel", "error", "-y", "-i", src}
	for i, s := range info.Subs {
		args = append(args, "-map", fmt.Sprintf("0:s:%d", s.Index), "-f", "webvtt", path(i)+".tmp")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	for i := range info.Subs {
		if err == nil {
			os.Rename(path(i)+".tmp", path(i))
		} else {
			os.Remove(path(i) + ".tmp")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("extraindo legendas de %s: %v %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(path(n))
}

var vttTime = regexp.MustCompile(`^((?:\d+:)?\d{2}:\d{2}\.\d{3}) --> ((?:\d+:)?\d{2}:\d{2}\.\d{3})(.*)$`)

// shiftVTT adianta todas as falas em shift segundos e tira as que já passaram.
func shiftVTT(vtt []byte, shift float64) []byte {
	if shift <= 0 {
		return vtt
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(vtt))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	skip := false // dentro de uma fala que já passou (até a linha em branco que fecha ela)
	for sc.Scan() {
		line := sc.Text()
		if m := vttTime.FindStringSubmatch(line); m != nil {
			start, end := parseVTTTime(m[1])-shift, parseVTTTime(m[2])-shift
			skip = end <= 0
			if !skip {
				fmt.Fprintf(&out, "%s --> %s%s\n", fmtVTTTime(max(start, 0)), fmtVTTTime(end), m[3])
			}
			continue
		}
		if skip {
			skip = line != ""
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func parseVTTTime(s string) float64 {
	var secs float64
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		secs = secs*60 + v
	}
	return secs
}

func fmtVTTTime(secs float64) string {
	ms := int64(secs*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
