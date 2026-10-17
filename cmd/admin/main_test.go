package main

import "testing"

func TestPrivateAdminCommandAllowlist(t *testing.T) {
	const id = "123e4567-e89b-42d3-a456-426614174000"
	for _, args := range [][]string{
		{"issue", "merchant-a", id},
		{"rotate", id, id},
		{"revoke", id, id},
		{"recover", id},
		{"backfill"},
		{"shell", "-c", "echo secret"},
		{"rotate", "x;echo secret", id, "--confirm"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted unauthorized or unconfirmed command: %v", args)
		}
	}
	for _, args := range [][]string{
		{"issue", "merchant-a", id, "--confirm"},
		{"rotate", id, id, "--confirm"},
		{"revoke", id, id, "--confirm"},
		{"recover", id, "--confirm"},
		{"backfill", "--confirm"},
		{"inspect", id},
	} {
		if _, err := parse(args); err != nil {
			t.Fatalf("rejected allowed command %v: %v", args, err)
		}
	}
}
