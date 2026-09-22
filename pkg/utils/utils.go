package utils

import (
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// RandomString returns a string of random alpha characters of the specified
// length.
func RandomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	lenLetters := len(letters)
	for i := 0; i < n; i++ {
		b[i] = letters[rand.Intn(lenLetters)]
	}
	return string(b)
}

// RandomStringFast returns a random string containing printable ascii
// characters: [0-9a-zA-Z_-]{n}. Each character encodes 6bits of entropy. To
// avoid wasting entropy, it is better to create a string whose length is a
// multiple of 10. For instance a 20 bytes string will encode 120 bits of
// entropy.
func RandomStringFast(rng *rand.Rand, n int) string {
	// extract 10 letters — 60 bits of entropy — for each pseudo-random uint64
	const K = 10
	const L = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-"
	b := make([]byte, ((n+K-1)/K)*K)
	for i := 0; i < n; i += K {
		rn := rng.Uint64()
		for j := 0; j < K; j++ {
			b[i+j] = L[rn&0x3F]
			rn >>= 6
		}
	}
	return string(b[:n])
}

// IsInArray returns whether or not a string is in the given array of strings.
func IsInArray(s string, a []string) bool {
	for _, ss := range a {
		if s == ss {
			return true
		}
	}
	return false
}

// StripPort extract the domain name from a domain:port string.
func StripPort(domain string) string {
	if strings.Contains(domain, ":") {
		cleaned, _, err := net.SplitHostPort(domain)
		if err != nil {
			return domain
		}
		return cleaned
	}
	return domain
}

// CookieDomain removes the port and does IDNA encoding.
func CookieDomain(domain string) string {
	domain = StripPort(domain)
	ascii, err := idna.ToASCII(domain)
	if err != nil {
		return domain
	}
	return ascii
}

// NormalizeDomain normalizes a domain name by converting to lowercase,
// trimming spaces, and removing trailing dots.
func NormalizeDomain(domain string) string {
	domain = strings.TrimSpace(strings.ToLower(domain))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return ""
	}
	return domain
}

// NormalizeEmail lowercases and trims an email.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ExtractInstanceHost extracts and normalizes the host from an instance URL.
// It handles full URLs (https://example.com) and plain hostnames (example.com:8080),
// and strips the port from the result.
func ExtractInstanceHost(instanceURL string) string {
	if instanceURL == "" {
		return ""
	}

	// Try parsing as a full URL first
	u, err := url.Parse(instanceURL)
	if err == nil && u.Host != "" {
		// Successfully parsed as URL, extract host and strip port
		return NormalizeDomain(StripPort(u.Host))
	}

	// Fallback: treat as plain hostname (with optional path/port)
	host := instanceURL

	// Remove scheme if present but parsing failed
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}

	// Remove path
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}

	// Strip port and normalize
	return NormalizeDomain(StripPort(host))
}

// SplitTrimString slices s into all substrings a s separated by sep, like
// strings.Split. In addition it will trim all those substrings and filter out
// the empty ones.
func SplitTrimString(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	return TrimStrings(strings.Split(s, sep))
}

// TrimStrings trim all strings and filter out the empty ones.
func TrimStrings(strs []string) []string {
	filteredStrs := strs[:0]
	for _, part := range strs {
		part = strings.TrimSpace(part)
		if part != "" {
			filteredStrs = append(filteredStrs, part)
		}
	}
	return filteredStrs
}

// UniqueStrings returns a filtered slice without string duplicates.
func UniqueStrings(strs []string) []string {
	filteredStrs := strs[:0]
	for _, str1 := range strs {
		found := false
		for _, str2 := range filteredStrs {
			if str1 == str2 {
				found = true
				break
			}
		}
		if !found {
			filteredStrs = append(filteredStrs, str1)
		}
	}
	return filteredStrs
}

// FileExists returns whether or not the file exists on the current file
// system.
func FileExists(name string) (bool, error) {
	infos, err := os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if infos.IsDir() {
		return false, fmt.Errorf("Path %s is a directory", name)
	}
	return true, nil
}

// DirExists returns whether or not the directory exists on the current file
// system.
func DirExists(name string) (bool, error) {
	infos, err := os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !infos.IsDir() {
		return false, fmt.Errorf("Path %s is not a directory", name)
	}
	return true, nil
}

// If `s` does not end with `suffix`, it returns `suffix` appended to `s`, or just `s`
func EnsureHasSuffix(s, suffix string) string {
	if strings.HasSuffix(s, suffix) {
		return s
	}
	return s + suffix
}

// AbsPath returns an absolute path relative.
func AbsPath(inPath string) string {
	if strings.HasPrefix(inPath, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		inPath = home + inPath[len("~"):]
	} else if strings.HasPrefix(inPath, "$HOME") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		inPath = home + inPath[len("$HOME"):]
	}

	if strings.HasPrefix(inPath, "$") {
		end := strings.Index(inPath, string(os.PathSeparator))
		inPath = os.Getenv(inPath[1:end]) + inPath[end:]
	}

	p, err := filepath.Abs(inPath)
	if err == nil {
		return filepath.Clean(p)
	}

	return ""
}

// CleanUTF8 returns a string with only valid UTF-8 runes
func CleanUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	v := make([]rune, 0, len(s))
	for _, r := range s {
		if r != utf8.RuneError {
			v = append(v, r)
		}
	}
	return string(v)
}

// CloneURL clones the given url
func CloneURL(u *url.URL) *url.URL {
	clone := *u
	if clone.User != nil {
		tmp := *clone.User
		clone.User = &tmp
	}
	return &clone
}

// DurationFuzzing returns a duration that is near the given duration, but
// randomized to avoid patterns like several cache entries that expires at the
// same time.
func DurationFuzzing(d time.Duration, variation float64) time.Duration {
	if variation > 1.0 || variation < 0.0 {
		panic("DurationRandomized: variation should be between 0.0 and 1.0")
	}
	return time.Duration(float64(d) * (1.0 + variation*(2.0*rand.Float64()-1.0)))
}
