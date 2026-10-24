package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/schema"
	"github.com/module195/torrent-indexer/utils"
)

// Precompiled regexes shared by hot paths (compiled once, safe for concurrent use).
var (
	reAudioField      = regexp.MustCompile(`(?i)(?:[áa]udio|idioma|languages):\s*([A-Za-zÀ-ú,|/\s]+?)(?:\s*(?:tamanho|qualidade|duração|formato|legenda|$))`)
	reStartsWithDigit = regexp.MustCompile(`^\d`)
	reYearField       = regexp.MustCompile(`(?:Lançamento|Year): (.*)`)
	reFourDigits      = regexp.MustCompile(`\b(\d{4})\b`)
	reYearInParens    = regexp.MustCompile(`\((\d{4})\)`)
	reSizeGBMB        = regexp.MustCompile(`(\d+[\.,]?\d+) ?(GB|MB)`)
	reWhitespace      = regexp.MustCompile(`\s+`)
	reEpisodeMarkers  = regexp.MustCompile(`(?i)\s*(S\d{1,2}E\d{1,2}|S\d{1,2}|temporada\s*\d+|episodio\s*\d+|episódio\s*\d+|season\s*\d+|episode\s*\d+|ep\.?\s*\d+)\s*`)
	reYearWord        = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	reLeadingArticle  = regexp.MustCompile(`(?i)^(the|a|o|os|as|um|uma)\s+`)
	reSxxEyy          = regexp.MustCompile(`(?i)s(\d+)e(\d+)`)
	reIMDbID          = regexp.MustCompile(`^tt[0-9]{7,8}$`)
	reQualitySuffix   = regexp.MustCompile(`(?i)(s\d+e\d+|1080p|720p|480p|4k|web-dl|webrip|bluray|hdtv|dual|dublado|legendado).*$`)
	rePortugueseChars = regexp.MustCompile(`[áéíóúãõç]`)
)

// getDocument retrieves a document from the cache or makes a request to get it.
// It first checks the Redis cache for the document body.
func getDocument(ctx context.Context, i *Indexer, link, referer string) (*goquery.Document, error) {
	// try to get from redis first
	docKey := cache.Key(cache.KindDoc, link, link)
	docCache, err := i.redis.Get(ctx, docKey)
	if err == nil {
		i.metrics.CacheHits.WithLabelValues("document_body").Inc()
		logging.Debug().Str("url", link).Msg("Returning document from long-lived cache")
		return goquery.NewDocumentFromReader(io.NopCloser(bytes.NewReader(docCache)))
	}
	defer i.metrics.CacheMisses.WithLabelValues("document_body").Inc()

	resp, err := i.requester.GetDocument(ctx, link, referer)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	body, err := io.ReadAll(resp)
	if err != nil {
		return nil, err
	}

	// set cache
	err = i.redis.Set(ctx, docKey, body)
	if err != nil {
		logging.Error().Err(err).Str("url", link).Msg("Failed to set document body in redis cache")
	}

	doc, err := goquery.NewDocumentFromReader(io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}

	return doc, nil
}

func getPublishedDateFromMeta(document *goquery.Document) time.Time {
	var date time.Time
	//<meta property="article:published_time" content="2019-08-23T13:20:57+00:00">
	datePublished := strings.TrimSpace(document.Find("meta[property=\"article:published_time\"]").AttrOr("content", ""))
	if datePublished == "" {
		// <meta property="og:updated_time" content="2025-09-30T13:08:58-03:00">
		datePublished = strings.TrimSpace(document.Find("meta[property=\"og:updated_time\"]").AttrOr("content", ""))
	}

	if datePublished == "" {
		// type="application/ld+json" with "datePublished" attribute on json
		scriptTags := document.Find("script[type=\"application/ld+json\"]")
		scriptTags.EachWithBreak(func(i int, s *goquery.Selection) bool {
			scriptContent := s.Text()
			mapData := make(map[string]interface{})
			err := json.Unmarshal([]byte(scriptContent), &mapData)
			if err != nil {
				return true // continue
			}
			if dateVal, ok := mapData["datePublished"].(string); ok {
				datePublished = dateVal
				return false // break
			}
			return true // continue
		})
	}

	if datePublished != "" {
		date, _ = time.Parse(time.RFC3339, datePublished)
	}

	return date
}

type datePattern struct {
	regex  *regexp.Regexp
	layout string
}

var datePatterns = []datePattern{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}`), "2006-01-02"},
	{regexp.MustCompile(`\d{2}-\d{2}-\d{4}`), "02-01-2006"},
	{regexp.MustCompile(`\d{2}/\d{2}/\d{4}`), "02/01/2006"},
	// Release Date: 4, October
	{regexp.MustCompile(`\d{1,2},? [A-Za-z]+`), "2, January"},
	// Release Date: October 4, 2020
	{regexp.MustCompile(`[A-Za-z]+ \d{1,2},? \d{4}`), "January 2, 2006"},
}

// getPublishedDateFromRawString extracts the date from a raw string using predefined patterns.
func getPublishedDateFromRawString(dateStr string) time.Time {
	for _, p := range datePatterns {
		match := p.regex.FindString(dateStr)

		if match != "" {
			date, err := time.Parse(p.layout, match)
			if err == nil {
				return date.UTC()
			}
		}
	}

	return time.Time{}
}

// getSeparator returns the separator used in the string.
// It checks for common separators like "|", ",", "/", and " e "
func getSeparator(s string) string {
	if strings.Contains(s, "|") {
		return "|"
	} else if strings.Contains(s, ",") {
		return ","
	} else if strings.Contains(s, "/") {
		return "/"
	} else if strings.Contains(s, " e ") {
		return " e "
	}
	return " "
}

// findAudioFromText extracts audio languages from a given text.
// It looks for patterns like "Áudio: Português, Inglês" or "Idioma: Português, Inglês"
func findAudioFromText(text string) []schema.Audio {
	var audio []schema.Audio
	// Regex mais específico - captura apenas até o próximo campo ou fim
	re := reAudioField
	audioMatch := re.FindStringSubmatch(text)
	if len(audioMatch) > 1 {
		sep := getSeparator(audioMatch[1])
		langs_raw := strings.Split(audioMatch[1], sep)
		for _, lang := range langs_raw {
			lang = strings.TrimSpace(lang)
			// Ignora números e textos muito curtos
			if len(lang) < 3 || reStartsWithDigit.MatchString(lang) {
				continue
			}
			a := schema.GetAudioFromString(lang)
			if a != nil {
				audio = append(audio, *a)
			}
		}
	}
	return audio
}

// findYearFromText extracts the year from a given text.
// It looks for patterns like "Lançamento: 2001" in the title.
func findYearFromText(text string, title string) (year string) {
	re := reYearField
	yearMatch := re.FindStringSubmatch(text)
	if len(yearMatch) > 0 {
		lancamentoText := strings.TrimSpace(yearMatch[1])
		// Extract 4-digit year from the lançamento field
		yearRe := reFourDigits
		if yearDigits := yearRe.FindStringSubmatch(lancamentoText); len(yearDigits) > 0 {
			year = yearDigits[1]
		}
	}

	if year == "" {
		re = reYearInParens
		yearMatch := re.FindStringSubmatch(title)
		if len(yearMatch) > 0 {
			year = yearMatch[1]
		}
	}

	return year
}

// findSizesFromText extracts sizes from a given text.
// It looks for patterns like "Tamanho: 1.26 GB" or "Tamanho: 700 MB".
func findSizesFromText(text string) []string {
	var sizes []string
	// everything that ends with GB or MB, using ',' or '.' as decimal separator
	re := reSizeGBMB
	sizesMatch := re.FindAllStringSubmatch(text, -1)
	if len(sizesMatch) > 0 {
		for _, size := range sizesMatch {
			// "1,62 GB" -> "1.62 GB", the format the size parsers expect
			sizes = append(sizes, strings.Replace(size[0], ",", ".", 1))
		}
	}
	return sizes
}

// reReleaseNameSize matches a size written in a release name: "999MB",
// "1.6GB", "2,5 GB".
var reReleaseNameSize = regexp.MustCompile(`(?i)(?:^|[^0-9])(\d+(?:[.,]\d+)?) ?(gb|mb|gib|mib)(?:$|[^a-z0-9])`)

// releaseSize returns the size a magnet states for itself: its exact length
// (xl=) or, failing that, the only size written in its release name. Both
// belong to that torrent, unlike the sizes listed on the page.
func releaseSize(magnetLink, releaseTitle string) string {
	if m := reMagnetXL.FindStringSubmatch(magnetLink); len(m) > 1 {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			return utils.FormatBytes(n)
		}
	}
	matches := reReleaseNameSize.FindAllStringSubmatch(releaseTitle, -1)
	if len(matches) != 1 {
		return ""
	}
	unit := strings.ToUpper(matches[0][2][:1]) + "B"
	return strings.Replace(matches[0][1], ",", ".", 1) + " " + unit
}

var reMagnetXL = regexp.MustCompile(`[?&]xl=(\d+)`)

var imdbLinkRE = regexp.MustCompile(`https://www.imdb.com(/[a-z]{2})?/title/(tt\d+)/?`)
var subtitlesHintIMDBLinkRE = regexp.MustCompile(`imdbid-((tt)?\d+)`)

// getIMDBLink extracts the IMDB link from a given link.
// It looks for patterns like "https://www.imdb.com/title/tt1234567/".
// Returns an error if no valid IMDB link is found.
func getIMDBLink(link string) (string, error) {
	var imdbLink string

	matches := imdbLinkRE.FindStringSubmatch(link)
	if len(matches) > 0 {
		imdbLink = matches[0]
	} else if matches := subtitlesHintIMDBLinkRE.FindStringSubmatch(link); len(matches) > 0 {
		id := strings.TrimPrefix(matches[1], "tt")
		imdbLink = fmt.Sprintf("https://www.imdb.com/title/tt%s/", id)
	} else {
		return "", fmt.Errorf("no imdb link found")
	}
	return imdbLink, nil
}

// appendAudioISO639_2Code appends the audio languages to the title in ISO 639-2 code format.
// It formats the title to include the audio languages in parentheses.
// Example: "Movie Title (eng, por)"
func appendAudioISO639_2Code(title string, a []schema.Audio) string {
	if len(a) > 0 {
		title = stripAudioSuffix(title)
		audio := []string{}
		for _, lang := range a {
			audio = append(audio, lang.String())
		}
		audio = slices.Compact(audio)
		title = fmt.Sprintf("%s (%s)", title, strings.Join(audio, ", "))
	}
	return title
}

// normalizeForMatch normaliza texto para comparação (lowercase + remove acentos).
func normalizeForMatch(s string) string {
	s = strings.ToLower(s)
	replacements := map[string]string{
		"á": "a", "à": "a", "ã": "a", "â": "a",
		"é": "e", "ê": "e",
		"í": "i",
		"ó": "o", "õ": "o", "ô": "o",
		"ú": "u", "ü": "u",
		"ç": "c",
	}
	for from, to := range replacements {
		s = strings.ReplaceAll(s, from, to)
	}
	return s
}

// reAudioSuffix matches the "(brazilian, eng)" suffix appendAudioISO639_2Code adds.
var reAudioSuffix = regexp.MustCompile(`\s*\((?:[a-z]{3}|brazilian)(?:, ?(?:[a-z]{3}|brazilian))*\)\s*$`)

// stripAudioSuffix removes the audio tags appended by appendAudioISO639_2Code,
// so a title can be tagged again without counting its own tags as markers.
func stripAudioSuffix(title string) string {
	return reAudioSuffix.ReplaceAllString(title, "")
}

var reNonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// Release-name words that name an audio track. Keys are whole words of the
// normalized title (lowercase, no accents, punctuation turned into spaces).
var (
	ptAudioMarkers  = []string{"dual", "dublado", "dub", "multi", "nacional", "pt", "ptbr", "brazilian", "portugues", "portuguese"}
	subtitleMarkers = []string{"legendado", "leg", "legenda", "legendas"}
	otherAudioWords = map[string]schema.Audio{
		"eng": schema.AudioEnglish, "english": schema.AudioEnglish, "ingles": schema.AudioEnglish,
		"ita": schema.AudioItalian, "italian": schema.AudioItalian, "italiano": schema.AudioItalian,
		"kor": schema.AudioKorean, "korean": schema.AudioKorean, "coreano": schema.AudioKorean,
		"jap": schema.AudioJapanese, "japanese": schema.AudioJapanese, "japones": schema.AudioJapanese,
		"spa": schema.AudioSpanish, "spanish": schema.AudioSpanish, "espanhol": schema.AudioSpanish,
		"fre": schema.AudioFrench, "french": schema.AudioFrench, "frances": schema.AudioFrench,
	}
)

// getAudioFromTitle extracts audio languages from the release title.
// The audio found on the page (audioFromContent) describes the whole post, and a
// post often lists dubbed and subtitled releases together, so markers in the
// release name win over it:
//   - NACIONAL: Portuguese only (Brazilian production);
//   - DUAL, DUBLADO, PT-BR...: Portuguese plus the page audio;
//   - LEGENDADO or a named foreign language (KOREAN, ITA ENG) without a
//     Portuguese marker: the page's Portuguese is dropped.
//
// Without markers, the page audio is kept as is.
func getAudioFromTitle(releaseTitle string, audioFromContent []schema.Audio) []schema.Audio {
	words := " " + strings.TrimSpace(reNonAlphanumeric.ReplaceAllString(normalizeForMatch(stripAudioSuffix(releaseTitle)), " ")) + " "
	has := func(markers ...string) bool {
		for _, m := range markers {
			if strings.Contains(words, " "+m+" ") {
				return true
			}
		}
		return false
	}
	var named []schema.Audio
	for w, a := range otherAudioWords {
		if strings.Contains(words, " "+w+" ") {
			named = append(named, a)
		}
	}
	isPT := func(a schema.Audio) bool { return a.String() == schema.Audio(schema.AudioPortuguese).String() }

	magnetAudio := append([]schema.Audio{}, audioFromContent...)
	switch {
	case has("nacional"):
		magnetAudio = []schema.Audio{schema.AudioPortuguese}
	case has(ptAudioMarkers...):
		magnetAudio = append(magnetAudio, schema.AudioPortuguese)
		magnetAudio = append(magnetAudio, named...)
	case has(subtitleMarkers...) || len(named) > 0:
		magnetAudio = slices.DeleteFunc(magnetAudio, isPT)
		magnetAudio = append(magnetAudio, named...)
		if len(magnetAudio) == 0 {
			magnetAudio = append(magnetAudio, schema.AudioEnglish)
		}
	}

	// Dedup e ordena
	slices.SortFunc(magnetAudio, func(a, b schema.Audio) int {
		return strings.Compare(a.String(), b.String())
	})
	magnetAudio = slices.CompactFunc(magnetAudio, func(a, b schema.Audio) bool {
		return a.String() == b.String()
	})

	return magnetAudio
}

// buildSmartFallbackQueries gera fallbacks inteligentes.
// Prioridade: 1) remover SxxEyy/temporada, 2) não encurtar abaixo de 3 palavras.
// Não corta o título no meio: nas buscas do WordPress, um pedaço do título que
// já não achou nada também não acha ("Outlander: Blood of My").
func buildSmartFallbackQueries(original string) []string {
	var out []string
	seen := map[string]bool{}

	add := func(s string) {
		s = strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
		if s == "" || s == original || seen[s] || len(strings.Fields(s)) < 3 {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	// 1. Remove apenas padrões de episódio/temporada
	noEp := reEpisodeMarkers.ReplaceAllString(original, " ")
	add(noEp)

	// 2. Remove ano
	noYear := reYearWord.ReplaceAllString(noEp, "")
	add(noYear)

	// 3. Remove artigos fracos (só se sobrar ≥ 3 palavras)
	noArticle := reLeadingArticle.ReplaceAllString(noYear, "")
	add(noArticle)

	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// reReleaseTagPrefix casa uma tag de qualidade/áudio no início do nome, como no
// dn do baixarapido: "WEB-DL 2160P SEM-COMPACTAÇÃO MKV 5 1 -DUBLADO-DUAL-AUDIO- Homem Aranha ...".
var reReleaseTagPrefix = regexp.MustCompile(`(?i)^[-.\s]*(WEB[\s-]?DL|WEB[\s-]?RIP|1080P|720P|4K|2160P|FULL[\s-]?HD|SEM[\s-]*COMPACTA[CÇ][AÃ]O|SEM[\s-]*COMPRESS[AÃ]O|MKV|MP4|X264|X265|HEVC|5[\s.]?1|DUBLADO|DUAL[\s-]?AUDIO|LEGENDADO|AAC|AC3|DTS|H264|H265)\s*[-.\s]*`)

// stripReleaseTagPrefix tira as tags de qualidade/áudio que alguns sites põem antes do título.
func stripReleaseTagPrefix(s string) string {
	for i := 0; i < 10; i++ {
		prev := s
		s = reReleaseTagPrefix.ReplaceAllString(s, "")
		if s == prev {
			break
		}
	}
	return s
}
