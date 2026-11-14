package expose

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// A share's tag says who made it: `nfpreview:<instance>:<key>`.
//
// The instance is a random id one installation generates once and keeps in its database. Without
// it the tag said only "some nfpreview made this", and a reap is "delete every share of ours the
// database does not list" — so a second daemon on the same zrok environment or Frontdoor tenant,
// started with an empty database, deleted every live share of the first.
//
// Three spellings exist on a controller:
//
//	nfpreview:<instance>:<key>   stamped, written by this release onwards
//	nfpreview:<key>              unstamped, written up to v0.5.1
//	docpreview:<key>             unstamped, written up to v0.4.9
//
// A publication key is a preview id or `<preview id>/<build id>` and never holds a colon, which
// is what lets the stamped and unstamped spellings be told apart.

// NewInstanceID returns a fresh installation id: twelve hex characters, so it never holds the
// colon that separates it from the key.
func NewInstanceID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidInstanceID reports whether s can be written into a tag and read back: non-empty, short,
// and made only of characters that cannot be mistaken for a separator.
func ValidInstanceID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// stampTag writes the tag for a publication key. An empty instance writes the unstamped spelling.
func stampTag(instance, key string) string {
	if instance == "" {
		return targetPrefix + key
	}
	return targetPrefix + instance + ":" + key
}

// parseTag reads a share's tag. ours is false for anything this program did not write, under
// either of its names; instance is empty for an unstamped tag.
func parseTag(tag string) (instance, key string, ours bool) {
	if rest, ok := strings.CutPrefix(tag, targetPrefix); ok {
		if inst, k, stamped := strings.Cut(rest, ":"); stamped {
			if !ValidInstanceID(inst) || k == "" || strings.Contains(k, ":") {
				// Our prefix on something that is not our shape. Nobody's to delete.
				return "", "", false
			}
			return inst, k, true
		}
		return "", rest, true
	}
	if rest, ok := strings.CutPrefix(tag, legacyTargetPrefix); ok {
		if strings.Contains(rest, ":") {
			return "", "", false
		}
		return "", rest, true
	}
	return "", "", false
}

// reapable reports whether a share with this tag may be deleted by the installation `mine` when
// nothing claims its key.
//
// Only a share carrying this installation's own stamp. A share stamped by another installation
// is that installation's, and an unstamped one cannot be attributed at all — it may be this
// installation's from before the upgrade or another's, and guessing wrong deletes somebody's
// live preview. An exposer with no instance set owns the unstamped spelling it writes, and
// nothing else.
func reapable(mine, tag string) (key string, ok bool) {
	instance, key, ours := parseTag(tag)
	if !ours || instance != mine {
		return "", false
	}
	// A legacy docpreview: tag reaches here only when mine is empty, as an unstamped one does.
	return key, true
}

// reclaimable reports whether a share with this tag may be deleted to free the name it holds for
// a preview this installation is publishing: its own, or an unstamped one. Never one another
// installation stamped.
func reclaimable(mine, tag string) bool {
	instance, _, ours := parseTag(tag)
	return ours && (instance == "" || instance == mine)
}

// doomedShare is the whole of a reap's decision for one listed share that this process is not
// serving: it is this installation's, and nothing in keep claims its key.
func doomedShare(mine, tag string, keep map[string]bool) bool {
	key, ok := reapable(mine, tag)
	return ok && !keep[key]
}
