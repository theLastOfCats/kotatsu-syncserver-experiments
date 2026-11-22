package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Move the deadline of a real, delivered token without sleeping for 15 minutes.
// This is reference-only setup; acceptance of the token is checked via HTTP.
func (r *run) expireReset(email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	label, err := exec.CommandContext(ctx, "podman", "container", "inspect", "--format",
		`{{ index .Config.Labels "io.kotatsu.contract-fixture" }}`, "--", r.referenceDB).Output()
	r.require(err == nil && strings.TrimSpace(string(label)) == "observations", "refusing to modify an unlabelled reference fixture")

	// The hex literal keeps the address data separate from SQL syntax.
	query := fmt.Sprintf(`UPDATE users SET password_reset_token_expires_at = 1
WHERE email = 0x%x AND password_reset_token_hash IS NOT NULL;
SELECT ROW_COUNT();`, email)
	result, err := exec.CommandContext(ctx, "podman", "exec", "-e", "MYSQL_PWD=local-contract-only", "--", r.referenceDB,
		"mariadb", "--protocol=tcp", "-h", "127.0.0.1", "-u", "kotatsu", "--batch", "--skip-column-names",
		"kotatsu_contract", "-e", query).Output()
	r.require(err == nil && strings.TrimSpace(string(result)) == "1", "could not expire exactly one issued token in the reference fixture")
}
