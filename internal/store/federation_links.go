package store

import (
	"context"
	"fmt"
	"time"
)

type FederationLink struct {
	HubArchiveUID string    `json:"hub_archive_uid"`
	HubOrigin     string    `json:"hub_origin"`
	EnrollmentUID string    `json:"enrollment_uid"`
	Member        string    `json:"member"`
	PolicyJSON    string    `json:"policy_json"`
	PolicyDigest  string    `json:"policy_digest"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Store) CreateFederationLinkContext(ctx context.Context, link FederationLink) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO federation_links (hub_archive_uid, hub_origin, enrollment_uid, member_handle, policy_json, policy_digest) VALUES (?, ?, ?, ?, ?, ?)`), link.HubArchiveUID, link.HubOrigin, link.EnrollmentUID, link.Member, link.PolicyJSON, link.PolicyDigest)
	if err != nil {
		return fmt.Errorf("create federation link: %w", err)
	}
	return nil
}

func (s *Store) ListFederationLinksContext(ctx context.Context) ([]FederationLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hub_archive_uid, hub_origin, enrollment_uid, member_handle, policy_json, policy_digest, state, created_at FROM federation_links ORDER BY created_at, hub_archive_uid`)
	if err != nil {
		return nil, fmt.Errorf("list federation links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	links := []FederationLink{}
	for rows.Next() {
		var l FederationLink
		if err := rows.Scan(&l.HubArchiveUID, &l.HubOrigin, &l.EnrollmentUID, &l.Member, &l.PolicyJSON, &l.PolicyDigest, &l.State, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan federation link: %w", err)
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func (s *Store) SetFederationLinkStateContext(ctx context.Context, uid, state string) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE federation_links SET state = ? WHERE hub_archive_uid = ?`), state, uid)
	return err
}

func (s *Store) DeleteFederationLinkContext(ctx context.Context, uid string) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM federation_links WHERE hub_archive_uid = ?`), uid)
	return err
}
