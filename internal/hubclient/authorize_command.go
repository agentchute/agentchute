package hubclient

import "strings"

// HubAuthorizeCommand is the one spelling of the "run this ON THE HUB" command
// every remedy prints. An operator pastes it into a shell on the hub, so every
// argument is single-quoted: the pool was printed bare, and a crafted URL path
// turned the paste into a shell command (review 2026-10-08, S8). ParseRemoteURL
// now refuses such paths too; quoting here keeps a pool reported by the hub, or
// a key comment, from mattering either.
func HubAuthorizeCommand(agentID, pool, pubkey string, replace bool) string {
	cmd := "agentchute hub authorize --agent " + shellQuote(agentID) + " --pool " + shellQuote(pool) + " --key " + shellQuote(pubkey)
	if replace {
		cmd += " --replace-key"
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
