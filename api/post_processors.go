package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/schema"
	"github.com/module195/torrent-indexer/utils"
	"github.com/hbollon/go-edlib"
)

// CleanupTitleWebsites removes unwanted characters from the title
func CleanupTitleWebsites(_ *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	for i := range torrents {
		torrents[i].Title = utils.RemoveKnownWebsites(torrents[i].Title)
	}
	return torrents
}

func AppendAudioTags(_ *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	for i, it := range torrents {
		// skip if title is empty
		if it.Title == "" {
			continue
		}
		// reprocess audio tags in case any middleware has changed the title
		torrents[i].Audio = getAudioFromTitle(it.Title, it.Audio)

		// for each video file, get audio ISO639-2 code and append to title
		for _, file := range it.Files {
			// check if file is a video
			if !utils.IsVideoFile(file.Path) {
				continue
			}
			torrents[i].Audio = getAudioFromTitle(file.Path, torrents[i].Audio)
		}

		torrents[i].Title = appendAudioISO639_2Code(torrents[i].Title, torrents[i].Audio)
	}

	return torrents
}

// SendToSearchIndexer sends the indexed torrents to the search indexer
func SendToSearchIndexer(i *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	// nothing to do when Meilisearch is not configured or there is nothing to index
	if i.search == nil || i.search.BaseURL == "" || len(torrents) == 0 {
		return torrents
	}
	go func() {
		_ = i.search.IndexTorrents(torrents)
	}()
	return torrents
}

// FullfilMissingMetadata fills in missing metadata for indexed torrents
func FullfilMissingMetadata(i *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	if !i.magnetMetadataAPI.IsEnabled() {
		return torrents
	}

	return utils.ParallelFlatMap(torrents, func(it schema.IndexedTorrent) ([]schema.IndexedTorrent, error) {
		if it.Size != "" && it.Title != "" && it.OriginalTitle != "" {
			return []schema.IndexedTorrent{it}, nil
		}
		m, err := i.magnetMetadataAPI.FetchMetadata(r.Context(), it.MagnetLink)
		if err != nil {
			return []schema.IndexedTorrent{it}, nil
		}

		// convert size in bytes to a human-readable format
		it.Size = utils.FormatBytes(m.Size)

		// Use name from metadata if available as it is more accurate
		if m.Name != "" {
			it.Title = m.Name
		}
		logging.Debug().Str("info_hash", m.InfoHash).Str("size", it.Size).Msg("Retrieved metadata from MagnetMetadataAPI")

		// If files are present, add them to the indexed torrent
		if len(m.Files) > 0 {
			it.Files = make([]schema.File, len(m.Files))
			for i, file := range m.Files {
				it.Files[i] = schema.File{
					Path: file.Path,
					Size: utils.FormatBytes(file.Size),
				}
			}
		}

		// If "date" is zero, use the date from metadata if available
		if it.Date.IsZero() {
			it.Date = m.CreatedAt
		}

		return []schema.IndexedTorrent{it}, nil
	})
}

func FallbackPostTitle(i *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	emptyTitles := 0

	for idx := range torrents {
		if torrents[idx].Title == "" {
			if i.config.FallbackTitleEnabled {
				torrents[idx].Title = fmt.Sprintf("[UNSAFE] %s", torrents[idx].OriginalTitle)
			} else {
				emptyTitles++
			}
		}
	}

	if emptyTitles > 0 && !i.magnetMetadataAPI.IsEnabled() {
		logging.WarnWithRequest(r).
			Int("empty_titles", emptyTitles).
			Msg("Some torrents have empty titles. Consider setting up MAGNET_METADATA_API (recommended) or set FALLBACK_TITLE_ENABLED=true.")
	}

	return torrents
}

func AddSimilarityCheck(i *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	q := r.URL.Query().Get("q")
	qNorm := normalize(q)

	// Extract episode info from query (e.g. s01e05)
	epRegex := reSxxEyy
	qEpisode := epRegex.FindString(qNorm)

	// Query without episode for title matching
	qTitle := epRegex.ReplaceAllString(qNorm, "")
	qTitle = strings.TrimSpace(qTitle)

	for i, it := range torrents {
		// Extract main title (before year/quality patterns)
		mainTitle := extractMainTitle(it.Title)
		originalTitle := extractMainTitle(it.OriginalTitle)

		// Normalize for matching
		combined := normalize(mainTitle + " " + originalTitle)
		titleNorm := normalize(it.Title)

		// Compute similarity
		splitLength := 2
		sim := edlib.JaccardSimilarity(combined, qNorm, splitLength)

		// Check if main title matches (without episode); an empty title is a
		// substring of every query, so it never counts as a match
		titleMatches := combined != "" && (strings.Contains(combined, qTitle) || strings.HasPrefix(combined, qTitle) || strings.Contains(qTitle, combined))

		if titleMatches {
			if qEpisode != "" {
				// Query has episode: boost only if title has that episode
				if strings.Contains(titleNorm, qEpisode) {
					sim = max(sim, 0.5)
				}
			} else {
				// No episode in query: boost all matching titles
				sim = max(sim, 0.5)
			}
		}

		torrents[i].Similarity = sim
	}

	// Filter low similarity, unless filter_results=false
	// Skip filter for IMDB queries (tt1234567) — no semantic similarity
	imdbPattern := reIMDbID
	if q != "" && !imdbPattern.MatchString(strings.ToLower(q)) && filterResults(r) {
		threshold := defaultMinSimilarity
		if qEpisode != "" {
			// Query has episode: require either boost (0.5) or high base similarity
			threshold = 0.45
		}
		if minSim, ok := minSimilarityParam(r); ok {
			threshold = minSim
		}
		torrents = utils.Filter(torrents, func(it schema.IndexedTorrent) bool {
			// If query has episode, also check title contains the episode
			if qEpisode != "" {
				titleNorm := normalize(it.Title)
				hasEpisode := strings.Contains(titleNorm, qEpisode)
				// Must have episode match OR very high similarity
				return hasEpisode && it.Similarity >= threshold || it.Similarity >= max(threshold, 0.48)
			}
			return it.Similarity >= threshold
		})
	}

	// sort by similarity
	slices.SortFunc(torrents, func(i, j schema.IndexedTorrent) int {
		return int((j.Similarity - i.Similarity) * 1000)
	})

	return torrents
}

// defaultMinSimilarity drops what the sites' full-text search adds around the
// real hits (other shows that mention the title in the post body score up to
// ~0.29, close spellings and translations of the title score 0.4+).
const defaultMinSimilarity float32 = 0.35

// filterResults reads "filter_results"; only an explicit false turns the
// similarity filter off.
func filterResults(r *http.Request) bool {
	v, err := strconv.ParseBool(r.URL.Query().Get("filter_results"))
	return err != nil || v
}

// minSimilarityParam reads "min_similarity" (0 to 1).
func minSimilarityParam(r *http.Request) (float32, bool) {
	v, err := strconv.ParseFloat(r.URL.Query().Get("min_similarity"), 32)
	if err != nil || v < 0 || v > 1 {
		return 0, false
	}
	return float32(v), true
}

var accentReplacer = strings.NewReplacer(
	"á", "a", "à", "a", "ã", "a", "â", "a",
	"é", "e", "è", "e", "ê", "e",
	"í", "i", "ì", "i", "î", "i",
	"ó", "o", "ò", "o", "õ", "o", "ô", "o",
	"ú", "u", "ù", "u", "û", "u",
	"ç", "c",
)

// normalize removes accents, punctuation and lowercases
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = accentReplacer.Replace(s)
	return strings.TrimSpace(s)
}

// extractMainTitle extracts the main title before year/quality patterns
func extractMainTitle(title string) string {
	// Remove quality/year patterns: S01E01, 1080p, WEB-DL, etc.
	re := reQualitySuffix
	title = re.ReplaceAllString(title, "")
	// Remove year patterns
	reYear := reYearWord
	title = reYear.ReplaceAllString(title, "")
	return strings.TrimSpace(title)
}

// ApplyLimit limits the number of results based on the "limit" query parameter
func ApplyLimit(_ *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	limitStr := r.URL.Query().Get("limit")
	if limitStr == "" {
		return torrents
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		return torrents
	}

	if len(torrents) > limit {
		return torrents[:limit]
	}

	return torrents
}

// ApplySorting sorts the results based on "sortBy" and "sortDirection" query parameters
func ApplySorting(_ *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	slices.SortFunc(torrents, torrentComparator(r.URL.Query()))
	return torrents
}

// torrentComparator returns the ordering requested by the "sortBy" and
// "sortDirection" query parameters. Shared by ApplySorting and /indexers/multi.
func torrentComparator(params url.Values) func(i, j schema.IndexedTorrent) int {
	sortBy := params.Get("sortBy")
	q := params.Get("q")
	if sortBy == "" {
		if q == "" {
			sortBy = "date"
		} else {
			// for search queries, default to sorting by similarity
			sortBy = "similarity"
		}
	}

	sortDirection := params.Get("sortDirection")
	ascending := sortDirection == "asc"

	return func(i, j schema.IndexedTorrent) int {
		var cmp int
		switch sortBy {
		case "title":
			cmp = strings.Compare(strings.ToLower(i.Title), strings.ToLower(j.Title))
		case "original_title":
			cmp = strings.Compare(strings.ToLower(i.OriginalTitle), strings.ToLower(j.OriginalTitle))
		case "year":
			cmp = strings.Compare(i.Year, j.Year)
		case "date":
			if i.Date.Before(j.Date) {
				cmp = -1
			} else if i.Date.After(j.Date) {
				cmp = 1
			}
		case "seed_count", "seeders":
			cmp = i.SeedCount - j.SeedCount
		case "leech_count", "leechers":
			cmp = i.LeechCount - j.LeechCount
		case "size":
			// Parse size strings to bytes for accurate comparison
			iBytes := utils.ParseSize(i.Size)
			jBytes := utils.ParseSize(j.Size)
			if iBytes < jBytes {
				cmp = -1
			} else if iBytes > jBytes {
				cmp = 1
			}
		case "similarity":
			if i.Similarity < j.Similarity {
				cmp = -1
			} else if i.Similarity > j.Similarity {
				cmp = 1
			}
		default:
			return 0
		}

		if !ascending {
			cmp = -cmp
		}
		if cmp == 0 && sortBy != "seed_count" && sortBy != "seeders" {
			// ties go to the torrent with more seeds, whatever the direction
			cmp = j.SeedCount - i.SeedCount
		}
		return cmp
	}
}

// FilterBy filters the results based on various query parameters
func FilterBy(_ *Indexer, r *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	if r == nil {
		return torrents
	}
	// Parse filter parameters
	audioParam := r.URL.Query().Get("audio")
	yearParam := r.URL.Query().Get("year")
	imdbParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("imdb")))
	season := positiveIntParam(r, "season")
	episode := positiveIntParam(r, "episode")

	var requestedAudioTags []string
	if audioParam != "" {
		parts := strings.Split(audioParam, ",")
		for _, p := range parts {
			if t := strings.TrimSpace(strings.ToLower(p)); t != "" {
				requestedAudioTags = append(requestedAudioTags, t)
			}
		}
	}

	// accept a full imdb link too, and compare IDs exactly (tt1234567 != tt12345678)
	if id := reIMDbIDInLink.FindString(imdbParam); id != "" {
		imdbParam = id
	} else if id := reIMDbIDInLink.FindString("tt" + imdbParam); id != "" {
		imdbParam = id // bare digits: 0903747
	}

	// If no filters are active, return original list
	if len(requestedAudioTags) == 0 && yearParam == "" && imdbParam == "" && season == 0 && episode == 0 {
		return torrents
	}

	return utils.Filter(torrents, func(it schema.IndexedTorrent) bool {
		// Filter by Audio
		if len(requestedAudioTags) > 0 {
			hasAudio := false
			for _, audio := range it.Audio {
				if slices.Contains(requestedAudioTags, strings.ToLower(audio.String())) {
					hasAudio = true
					break
				}
			}
			if !hasAudio {
				return false
			}
		}

		// Filter by Year
		if yearParam != "" {
			if it.Year != yearParam {
				return false
			}
		}

		// Filter by IMDB ID
		if imdbParam != "" {
			if reIMDbIDInLink.FindString(strings.ToLower(it.IMDB)) != imdbParam {
				return false
			}
		}

		// Filter by season / episode
		if season > 0 || episode > 0 {
			info := parseReleaseInfo(it.Title)
			if info.empty() {
				info = parseReleaseInfo(it.OriginalTitle)
			}
			if !info.matches(season, episode) {
				return false
			}
		}

		return true
	})
}

// positiveIntParam reads a query param that must be a positive integer; 0 means absent or invalid.
func positiveIntParam(r *http.Request, name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var (
	// S04E10, S04E08-E10, S01E01-03
	reReleaseEpisode = regexp.MustCompile(`\bs(\d{1,2}) ?e(\d{1,3})(?: ?- ?e?(\d{1,3})\b)?`)
	// S01-S05, S01 a S05
	reReleaseSeasonRange = regexp.MustCompile(`\bs(\d{1,2}) ?(?:-|a|ao|ate|to) ?s?(\d{1,2})\b`)
	// S04
	reReleaseSeason = regexp.MustCompile(`\bs(\d{1,2})\b`)
	// 4ª Temporada, 4a temporada, 5 Temporada, 1ª a 5ª Temporada
	reReleaseNthSeason = regexp.MustCompile(`\b(\d{1,2}) ?(?:ª|º|°|a|o)? ?(?:(?:-|a|ao|ate) ?(\d{1,2}) ?(?:ª|º|°|a|o)? ?)?temporadas?\b`)
	// Temporada 4, Season 4, Seasons 1-5
	reReleaseSeasonWord = regexp.MustCompile(`\b(?:temporadas?|seasons?) ?(\d{1,2})(?: ?(?:-|a|ao|ate|to) ?(\d{1,2}))?\b`)
	// Todas as Temporadas, Série Completa, Complete Series
	reReleaseAllSeasons = regexp.MustCompile(`\btodas (?:as )?temporadas\b|\bserie completa\b|\bcomplete series\b`)
)

type releaseEpisodeRange struct{ season, from, to int }

// releaseInfo is the season/episode coverage a release title announces.
type releaseInfo struct {
	seasons  [][2]int // inclusive ranges
	episodes []releaseEpisodeRange
	all      bool
}

func parseReleaseInfo(title string) releaseInfo {
	s := accentReplacer.Replace(strings.ToLower(title))
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	s = reWhitespace.ReplaceAllString(s, " ")

	var info releaseInfo
	addSeasons := func(from, to string) {
		a, _ := strconv.Atoi(from)
		b, err := strconv.Atoi(to)
		if err != nil || b < a {
			b = a
		}
		if a > 0 {
			info.seasons = append(info.seasons, [2]int{a, b})
		}
	}
	for _, m := range reReleaseEpisode.FindAllStringSubmatch(s, -1) {
		season, _ := strconv.Atoi(m[1])
		from, _ := strconv.Atoi(m[2])
		to, err := strconv.Atoi(m[3])
		if err != nil || to < from {
			to = from
		}
		info.episodes = append(info.episodes, releaseEpisodeRange{season, from, to})
	}
	for _, re := range []*regexp.Regexp{reReleaseSeasonRange, reReleaseNthSeason, reReleaseSeasonWord} {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			addSeasons(m[1], m[2])
		}
	}
	for _, m := range reReleaseSeason.FindAllStringSubmatch(s, -1) {
		addSeasons(m[1], "")
	}
	info.all = reReleaseAllSeasons.MatchString(s)
	return info
}

func (ri releaseInfo) empty() bool {
	return len(ri.seasons) == 0 && len(ri.episodes) == 0 && !ri.all
}

func (ri releaseInfo) hasSeason(n int) bool {
	if ri.all {
		return true
	}
	for _, r := range ri.seasons {
		if r[0] <= n && n <= r[1] {
			return true
		}
	}
	for _, e := range ri.episodes {
		if e.season == n {
			return true
		}
	}
	return false
}

// matches reports whether the release holds season (0 = any) and episode (0 = any).
// A season pack holds every episode of its seasons; without a season, only
// explicit episodes (S01E10) can match an episode.
func (ri releaseInfo) matches(season, episode int) bool {
	if episode == 0 {
		return ri.hasSeason(season)
	}
	for _, e := range ri.episodes {
		if (season == 0 || e.season == season) && e.from <= episode && episode <= e.to {
			return true
		}
	}
	return season > 0 && len(ri.episodes) == 0 && ri.hasSeason(season)
}
