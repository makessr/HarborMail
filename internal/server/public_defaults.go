package server

import (
	"context"
	"database/sql"
)

const (
	publicMailboxQuotaMiB        = 1024
	publicAccountDailyRecipients = 100
	publicDomainDailyRecipients  = 500
)

func (a *app) seedMailboxLimits(ctx context.Context, tx *sql.Tx, owner string) error {
	domain := emailDomain(owner)
	quota := a.domainQuota[domain]
	if quota == 0 && a.publicDefaults {
		quota = publicMailboxQuotaMiB
	}
	if quota > 0 {
		suffix := " ON CONFLICT(owner) DO NOTHING"
		if a.dbDriver == "mysql" {
			suffix = " ON DUPLICATE KEY UPDATE limit_mib=limit_mib"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_quota(owner,used_bytes,limit_mib) VALUES(?,0,?)`+suffix, owner, quota); err != nil {
			return err
		}
	}
	if !a.publicDefaults {
		return nil
	}
	suffix := " ON CONFLICT(scope,target) DO NOTHING"
	if a.dbDriver == "mysql" {
		suffix = " ON DUPLICATE KEY UPDATE daily_limit=daily_limit"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO outbound_limits(scope,target,daily_limit,day,used)
		VALUES('account',?,?,0,0),('domain',?,?,0,0)`+suffix, owner, publicAccountDailyRecipients, domain, publicDomainDailyRecipients)
	return err
}
