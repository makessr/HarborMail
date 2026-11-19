package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

var errAdminMFA = errors.New("admin second factor is required or invalid")
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

type adminMFAConfig struct {
	Secret         string   `json:"secret"`
	RecoveryHashes []string `json:"recoveryHashes"`
}

type adminMFA struct {
	secret     []byte
	enrollment [32]byte
	recovery   map[[32]byte]bool
}

func parseAdminMFA(raw string, admins map[string]bool) (map[string]adminMFA, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var settings map[string]adminMFAConfig
	if err := json.Unmarshal([]byte(raw), &settings); err != nil || settings == nil {
		return nil, errors.New("HARBOR_ADMIN_MFA must be a JSON object")
	}
	result := make(map[string]adminMFA)
	seenSeeds := make(map[[32]byte]bool)
	for owner, item := range settings {
		if !admins[owner] {
			return nil, errors.New("HARBOR_ADMIN_MFA contains an address without administrative privileges")
		}
		seed, err := totpBase32.DecodeString(strings.ToUpper(item.Secret))
		if err != nil || len(seed) != 20 || len(item.RecoveryHashes) > 12 {
			return nil, errors.New("invalid administrator second-factor configuration")
		}
		entry := adminMFA{secret: seed, enrollment: sha256.Sum256(seed), recovery: make(map[[32]byte]bool)}
		if seenSeeds[entry.enrollment] {
			return nil, errors.New("each administrator must have a separate TOTP secret")
		}
		seenSeeds[entry.enrollment] = true
		for _, entryHash := range item.RecoveryHashes {
			digest, err := hex.DecodeString(entryHash)
			if err != nil || len(digest) != sha256.Size {
				return nil, errors.New("invalid administrator recovery code hash")
			}
			entry.recovery[[32]byte(digest)] = true
		}
		result[owner] = entry
	}
	return result, nil
}

func adminRecoveryHash(code string) [32]byte {
	return sha256.Sum256([]byte("Harbor Mail admin recovery\x00" + code))
}

func writeAdminMFAEnrollment(w io.Writer, owner string) error {
	if canonical, err := address(owner); err != nil || canonical != owner || strings.ToLower(owner) != owner {
		return errors.New("provide the lowercase administrator email address")
	}
	var seed [20]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return err
	}
	item := adminMFAConfig{Secret: totpBase32.EncodeToString(seed[:])}
	codes := make([]string, 8)
	for i := range codes {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		codes[i] = hex.EncodeToString(random[:])
		hash := adminRecoveryHash(codes[i])
		item.RecoveryHashes = append(item.RecoveryHashes, hex.EncodeToString(hash[:]))
	}
	params := url.Values{"secret": {item.Secret}, "issuer": {"Harbor Mail"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	uri := "otpauth://totp/" + url.PathEscape("Harbor Mail:"+owner) + "?" + params.Encode()
	config, err := json.MarshalIndent(map[string]adminMFAConfig{owner: item}, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Authenticator URI (secret):\n%s\n\nHARBOR_ADMIN_MFA configuration (secret):\n%s\n\nSingle-use recovery codes:\n%s\n",
		uri, config, strings.Join(codes, "\n"))
	return err
}

func adminTOTP(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 0x0f)
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}

func (a *app) consumeAdminMFA(ctx context.Context, tx *sql.Tx, owner, code string, now time.Time) error {
	entry, enabled := a.adminMFA[owner]
	if !enabled {
		return nil
	}
	code = strings.ToLower(strings.TrimSpace(code))
	var proof [32]byte
	var expires int64
	if len(code) == 6 {
		step := now.Unix() / 30
		found := false
		for candidate := step + 1; candidate >= step-1; candidate-- {
			if subtle.ConstantTimeCompare([]byte(code), []byte(adminTOTP(entry.secret, candidate))) == 1 {
				proof = sha256.Sum256(fmt.Appendf(nil, "Harbor Mail TOTP step\x00%d", candidate))
				expires = (candidate + 2) * 30
				found = true
				break
			}
		}
		if !found {
			return errAdminMFA
		}
	} else {
		proof = adminRecoveryHash(code)
		if len(code) != 32 || !entry.recovery[proof] {
			return errAdminMFA
		}
	}
	var used bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_mfa_used
		WHERE owner=? AND enrollment=? AND proof=?)`, owner, entry.enrollment[:], proof[:]).Scan(&used)
	if err != nil {
		return err
	}
	if used {
		return errAdminMFA
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_mfa_used WHERE owner=? AND expires>0 AND expires<=?`, owner, now.Unix()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_mfa_used(owner,enrollment,proof,expires) VALUES(?,?,?,?)`,
		owner, entry.enrollment[:], proof[:], expires)
	return err
}

func (a *app) webSessionCredential(owner string, base [32]byte) [32]byte {
	entry, enabled := a.adminMFA[owner]
	if !enabled {
		return base
	}
	mac := hmac.New(sha256.New, entry.secret)
	_, _ = mac.Write([]byte("Harbor Mail MFA browser session\x00"))
	_, _ = mac.Write(base[:])
	return [32]byte(mac.Sum(nil))
}

func (a *app) baseWebSessionCredential(ctx context.Context, db folderQueryer, owner string) ([32]byte, error) {
	base, err := a.baseAccountDigest(ctx, db, owner)
	return a.webSessionCredential(owner, base), err
}

func (a *app) removeStaleAdminSessions(ctx context.Context, privileged map[string]bool) error {
	for owner := range privileged {
		credential, err := a.baseWebSessionCredential(ctx, a.db, owner)
		if err != nil {
			return err
		}
		if _, err := a.db.ExecContext(ctx, `DELETE FROM sessions WHERE owner=? AND credential<>?`, owner, credential[:]); err != nil {
			return err
		}
	}
	return nil
}
