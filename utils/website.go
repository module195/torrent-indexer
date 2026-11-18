package utils

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var commonTLDs = []string{
	".com",
	".net",
	".org",
	".info",
	".biz",
	".co",
	".io",
	".xyz",
	".me",
	".tv",
	".cc",
	".us",
	".online",
	".site",
	".la",
	".se",
	".to",
}

var commonSubdomains = []string{
	"", // no prefix
	"www.",
}

var commonWebsiteSLDs = []string{
	"bludv",
	"torrentdosfilmes",
	"comando",
	"comandotorrents",
	"comandohds",
	"redetorrent",
	"torrenting",
	"baixarfilmesdubladosviatorrent",
	"hidratorrents",
	"wolverdonfilmes",
	"starckfilmes",
	"rapidotorrents",
	"sitedetorrents",
	"vamostorrent",
	"AZTORRENTS",
}

var websitePatterns = []string{
	`\[\s*ACESSE\s+%s\s*\]`,
	`\[?\s*%s(\s*\])?`,
}

// reAdPhrases matches the ads some release names carry around the site name:
// "[WWW.BLUDV.TV] Movie (4K BluRay) Acesse o ORIGINAL WWW.BLUDV.TV".
var reAdPhrases = regexp.MustCompile(`(?i)\bacesse\s+o\s+original\b`)

var regexesOnce sync.Once
var regexes []*regexp.Regexp

func getRegexes() []*regexp.Regexp {
	regexesOnce.Do(func() {
		var websites strings.Builder
		websites.WriteString("(?i)(")
		for _, prefix := range commonSubdomains {
			for _, name := range commonWebsiteSLDs {
				for _, tld := range commonTLDs {
					fmt.Fprintf(&websites, "%s%s%s|", prefix, name, tld)
				}
			}
		}
		// remove the last pipe character
		websites.WriteString(")")

		websitesStr := websites.String()
		websitesStr = strings.Replace(websitesStr, "|)", ")", 1)

		for _, pattern := range websitePatterns {
			regexes = append(regexes, regexp.MustCompile(fmt.Sprintf(pattern, websitesStr)))
		}
	})
	return regexes
}

// RemoveKnownWebsites removes known website patterns from the title.
// It uses a set of common prefixes, names, and TLDs to identify and remove
// website references from the title.
// It also removes any common patterns like "[ ACESSE bludv.com ]" or
// "[ bludv.se ]" or "bludv.xyz".
func RemoveKnownWebsites(title string) string {
	regexes := getRegexes()
	for _, re := range regexes {
		title = re.ReplaceAllString(title, "")
	}
	title = reAdPhrases.ReplaceAllString(title, "")
	// strings.Fields does not split on zero-width format characters, which
	// JavaScript's \s (guindex) does match: a run of U+FEFF in a magnet dn
	// survived here and hit quadratic regexes there (security audit 18/11)
	title = zeroWidthReplacer.Replace(title)
	title = strings.Join(strings.Fields(title), " ")
	return title
}

var zeroWidthReplacer = strings.NewReplacer(
	"\ufeff", " ", "\u200b", " ", "\u200c", " ", "\u200d", " ", "\u2060", " ", "\u180e", " ",
)

// MaxTitleRunes caps a release title. Real ones are far shorter; a scraped
// page controls the title (a magnet dn has no length limit).
const MaxTitleRunes = 512

// TruncateRunes returns s cut to at most n runes.
func TruncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
