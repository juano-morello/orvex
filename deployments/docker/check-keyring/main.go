// Command check-keyring verifies a preview backup before it is installed.
// The caller supplies the candidate on stdin and the preview database in
// ORVEX_DATABASE_URL. No key material or database error text is printed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := verify(ctx, os.Stdin, os.Getenv("ORVEX_DATABASE_URL"))
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verify(ctx context.Context, input io.Reader, dsn string) error {
	data, err := io.ReadAll(io.LimitReader(input, sealing.MaxKeyringFileBytes+1))
	defer clear(data)
	if err != nil || len(data) > sealing.MaxKeyringFileBytes {
		return errors.New("keyring: could not read backup within the size limit")
	}
	keys, err := sealing.ParseKeyring(data)
	if err != nil {
		return errors.New("keyring: invalid backup format")
	}

	dbErr := errors.New("keyring: could not check the preview database")
	if dsn == "" {
		return dbErr
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return dbErr
	}
	defer func() { _ = conn.Close(ctx) }()
	var sealedSchema bool
	err = conn.QueryRow(ctx, "SELECT to_regclass('orvex.round_secret_reveals') IS NOT NULL").Scan(&sealedSchema)
	if err != nil {
		return dbErr
	}
	if !sealedSchema {
		return nil
	}
	rows, err := conn.Query(ctx, `SELECT round_id, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext
		FROM orvex.round_secrets s WHERE secret_ciphertext IS NOT NULL AND NOT EXISTS
		(SELECT 1 FROM orvex.round_secret_reveals r WHERE r.round_id = s.round_id)`)
	if err != nil {
		return dbErr
	}
	defer rows.Close()
	for rows.Next() {
		var id [16]byte
		var sealed sealing.Sealed
		if err := rows.Scan(&id, &sealed.Scheme, &sealed.KeyID, &sealed.Nonce, &sealed.Ciphertext); err != nil {
			return dbErr
		}
		plaintext, err := keys.Open(id, sealed)
		clear(plaintext)
		if err != nil {
			return errors.New("keyring: backup cannot open an unsettled round")
		}
	}
	if rows.Err() != nil {
		return dbErr
	}
	return nil
}
