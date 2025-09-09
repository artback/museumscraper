package main

import "strings"

func getLastWord(s string) string {
	lastIndex := strings.LastIndex(s, " ")
	if lastIndex == -1 {
		return s
	}
	return s[lastIndex+1:]
}
