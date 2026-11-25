package notion

import "os"

func readLink(p string) (string, error) { return os.Readlink(p) }

// MsToTime formats a Unix-millisecond timestamp as RFC 3339 (UTC).
func MsToTime(ms int64) string { return msToISO(float64(ms)) }
