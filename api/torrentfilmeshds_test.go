package handler

import "testing"

func Test_releaseFromButtonLabel(t *testing.T) {
	page := "Ted Lasso 1ª Temporada (2020)"
	cases := []struct{ label, want string }{
		{"🧲S01.1080p | MKV | Ep.01/05 | Dual Áudio→", "Ted.Lasso.S01E01-E05.1080p.MKV.DUAL"},
		{"🧲S01.1080p | MKV | Ep.08 | Dual Áudio→", "Ted.Lasso.S01E08.1080p.MKV.DUAL"},
		{"🧲S01.1080p | MKV | Ep.10 Final | Dual Áudio→", "Ted.Lasso.S01E10.1080p.MKV.DUAL"},
		{"S3 720p", "Ted.Lasso.S03.720p"},
		{"Baixar", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := releaseFromButtonLabel(page, c.label); got != c.want {
			t.Errorf("releaseFromButtonLabel(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}

func Test_isGluedRelease(t *testing.T) {
	for _, s := range []string{"COMANDO.TO-TedLassoS01E01-05720p", "TedLassoS01E091080pDUAL", "TedLassoS04E07WEB-DL1080px264DUAL5.1"} {
		if !isGluedRelease(s) {
			t.Errorf("isGluedRelease(%q) = false", s)
		}
	}
	for _, s := range []string{"Ted.Lasso.S01E01.1080p.WEB-DL", "Ted Lasso S01E01-E05 720p", "Ted.Lasso.1080p"} {
		if isGluedRelease(s) {
			t.Errorf("isGluedRelease(%q) = true", s)
		}
	}
}
