package main

import "strings"

const profileEnv = "KSYNC_PROFILES"

func parseProfiles(raw string) []string {
	return normalizeProfiles(strings.Split(raw, ","))
}

func normalizeProfiles(entries []string) []string {
	var profiles []string
	for _, entry := range entries {
		if p := strings.TrimSpace(entry); p != "" {
			profiles = append(profiles, p)
		}
	}
	return profiles
}
