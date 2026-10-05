package update

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dittofleet/crosstalk/internal/release"
	"golang.org/x/term"
)

const (
	checkInterval = 24 * time.Hour
	fetchTimeout  = 2 * time.Second
)

func shouldCheck(version string) bool {
	if version == "dev" {
		return false
	}
	if os.Getenv("CI") != "" {
		return false
	}
	if os.Getenv("CROSSTALK_NO_UPDATE_CHECK") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func parseVersion(s string) []int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

func isNewer(latest, current string) bool {
	a := parseVersion(latest)
	b := parseVersion(current)
	for i := 0; i < max(len(a), len(b)); i++ {
		var ai, bi int
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if ai > bi {
			return true
		}
		if ai < bi {
			return false
		}
	}
	return false
}

func printHint(latest, current string) {
	fmt.Fprintf(os.Stderr,
		"\ncrosstalk %s is available (current: %s). Run `crosstalk update` to install.\n",
		latest, current,
	)
}

// MaybeCheck does a daily best-effort GitHub API ping for a newer release
// and prints a stderr hint when one exists. Errors are silently swallowed,
// since this is informational output and never fails a command.
func MaybeCheck(version string) {
	if !shouldCheck(version) {
		return
	}

	cache := LoadCache()
	now := time.Now().UnixMilli()

	hinted := cache != nil && isNewer(cache.LatestVersion, version)
	if hinted {
		printHint(cache.LatestVersion, version)
	}

	if cache != nil && now-cache.LastCheck < checkInterval.Milliseconds() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	latest, err := release.FetchLatestTag(ctx)
	if err != nil {
		// The attempt is recorded all the same, with what was known
		// before. Otherwise every command would wait on GitHub again for
		// as long as it cannot be reached.
		known := ""
		if cache != nil {
			known = cache.LatestVersion
		}
		_ = SaveCache(&Cache{LastCheck: now, LatestVersion: known})
		return
	}
	_ = SaveCache(&Cache{LastCheck: now, LatestVersion: latest})
	if !hinted && isNewer(latest, version) {
		printHint(latest, version)
	}
}
