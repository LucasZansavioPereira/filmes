package main

import "testing"

func TestParseName(t *testing.T) {
	cases := []struct {
		in, title, year string
		dir, series     bool
	}{
		{"The.Matrix.1999.1080p.BluRay.x264-GRP", "The Matrix", "1999", true, false},
		{"Breaking.Bad.S01.720p.WEB-DL.DUAL", "Breaking Bad", "", true, true},
		{"[TGx] Cidade de Deus (2002) Nacional 1080p.mp4", "Cidade de Deus", "2002", false, false},
		{"2001.A.Space.Odyssey.1968.1080p", "2001 A Space Odyssey", "1968", true, false},
		{"Dark.S02E05.Lost.and.Found.1080p.mkv", "Dark", "", false, true},
		{"The Office (US) Season 3 Complete", "The Office (US)", "", true, true},
		{"La Casa de Papel 1ª Temporada Dublado", "La Casa de Papel 1ª Temporada", "", true, false},
		{"Oppenheimer (2023) [1080p] [WEBRip] [5.1] [YTS.MX]", "Oppenheimer", "2023", true, false},
		{"Filme Caseiro.mp4", "Filme Caseiro", "", false, false},
		{"1917.2019.2160p.UHD", "1917", "2019", true, false},
	}
	for _, c := range cases {
		title, year, series := parseName(c.in, c.dir)
		if title != c.title || year != c.year || series != c.series {
			t.Errorf("parseName(%q) = %q, %q, %v; quero %q, %q, %v", c.in, title, year, series, c.title, c.year, c.series)
		}
	}
	for in, want := range map[string][2]int{"Show.S01E02.mkv": {1, 2}, "show 3x07 nome.mkv": {3, 7}, "Filme.2019.mkv": {0, 0}} {
		s, e := parseEpisode(in)
		if s != want[0] || e != want[1] {
			t.Errorf("parseEpisode(%q) = %d, %d", in, s, e)
		}
	}
}

func TestShiftVTT(t *testing.T) {
	in := "WEBVTT\n\n00:16.141 --> 00:19.144\nantes\n\n01:00:05.000 --> 01:00:07.500 align:start\nlinha 1\nlinha 2\n\n"
	want := "WEBVTT\n\n00:59:45.000 --> 00:59:47.500 align:start\nlinha 1\nlinha 2\n\n"
	if got := string(shiftVTT([]byte(in), 20)); got != want {
		t.Errorf("shiftVTT:\n%q\nquero\n%q", got, want)
	}
}
