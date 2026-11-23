//go:build live_highlights

package handlers

import (
    "testing"
    "time"
)

// External-network smoke run only, intentionally excluded from normal unit
// tests. These IDs are examples published by third-party resources and may be
// unavailable or outdated; the result is evidence, not a release gate.
func TestLivePublicHighlights(t *testing.T) {
    ids := []string{
        "18212783421286344", // URL shown in a published example
        "18023864549864045", // example from public scraper documentation
        "18195781759377100", // another documented example
    }
    t.Logf("time_utc=%s", time.Now().UTC().Format(time.RFC3339))
    found := 0
    for _, id := range ids {
        t.Run(id, func(t *testing.T) {
            item, err := GetHighlightPreviewNoAuth(id)
            if err != nil {
                t.Logf("UNAVAILABLE public cover: %v", err)
                return
            }
            if item == nil || len(item.Medias) == 0 || item.Username == "" {
                t.Log("UNAVAILABLE empty or unattributed highlight")
                return
            }
            t.Logf("AVAILABLE public cover: media_count=%d username_present=true",len(item.Medias))
            found++
        })
    }
    if found == 0 {
        t.Fatal("no tested public Highlight cover could be resolved anonymously from the CI runner; do not enable public Highlights yet")
    }
}
