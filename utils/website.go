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
	title = strings.Join(strings.Fields(title), " ")
	return title
}
