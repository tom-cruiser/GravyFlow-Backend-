package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type DeploymentDomainRecord struct {
	ID                string     `json:"id"`
	DeploymentID      string     `json:"deploymentId"`
	ProjectID         string     `json:"projectId"`
	CustomDomain      string     `json:"customDomain"`
	Verified          bool       `json:"verified"`
	VerificationToken string     `json:"verificationToken,omitempty"`
	VerifiedAt        *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func normalizeCustomDomain(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}

	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
			value = parsed.Host
		}
	}

	value = strings.TrimPrefix(value, "www.")
	value = strings.TrimSuffix(value, ".")
	return strings.TrimSpace(value)
}

func verifyTXTChallengeName(domain string) string {
	domain = normalizeCustomDomain(domain)
	if domain == "" {
		return ""
	}
	return "_gravyflow-verify." + domain
}

func (s *DeploymentStore) UpsertDeploymentDomain(ctx context.Context, userID string, deploymentID string, customDomain string) (DeploymentDomainRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentDomainRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	customDomain = normalizeCustomDomain(customDomain)
	if userID == "" || deploymentID == "" || customDomain == "" {
		return DeploymentDomainRecord{}, fmt.Errorf("userID, deploymentID, and customDomain are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return DeploymentDomainRecord{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("begin domain transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingDeploymentID string
	err = tx.QueryRow(ctx, `
SELECT deployment_id::text
FROM domains
WHERE custom_domain = $1
FOR UPDATE
`, customDomain).Scan(&existingDeploymentID)
	if err == nil {
		if strings.TrimSpace(existingDeploymentID) != deploymentID {
			return DeploymentDomainRecord{}, fmt.Errorf("custom domain is already attached to another deployment")
		}

		verificationToken, err := generateRandomToken(12)
		if err != nil {
			return DeploymentDomainRecord{}, err
		}

		var record DeploymentDomainRecord
		if err := tx.QueryRow(ctx, `
UPDATE domains
SET verified = FALSE,
	verification_token = $2,
	verified_at = NULL,
	updated_at = now()
WHERE custom_domain = $1
RETURNING id::text, deployment_id::text, project_id::text, custom_domain, verified, verification_token, verified_at, created_at, updated_at
`, customDomain, verificationToken).Scan(
			&record.ID,
			&record.DeploymentID,
			&record.ProjectID,
			&record.CustomDomain,
			&record.Verified,
			&record.VerificationToken,
			&record.VerifiedAt,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return DeploymentDomainRecord{}, fmt.Errorf("update deployment domain: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return DeploymentDomainRecord{}, fmt.Errorf("commit deployment domain update: %w", err)
		}
		return record, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeploymentDomainRecord{}, fmt.Errorf("lookup existing domain: %w", err)
	}

	verificationToken, err := generateRandomToken(12)
	if err != nil {
		return DeploymentDomainRecord{}, err
	}

	var record DeploymentDomainRecord
	if err := tx.QueryRow(ctx, `
INSERT INTO domains (
	project_id,
	deployment_id,
	app_id,
	custom_domain,
	verified,
	verification_token,
	verified_at
) VALUES (
	(SELECT project_id FROM deployments WHERE id = $1),
	$1,
	$1,
	$2,
	FALSE,
	$3,
	NULL
)
RETURNING id::text, deployment_id::text, project_id::text, custom_domain, verified, verification_token, verified_at, created_at, updated_at
`, deploymentID, customDomain, verificationToken).Scan(
		&record.ID,
		&record.DeploymentID,
		&record.ProjectID,
		&record.CustomDomain,
		&record.Verified,
		&record.VerificationToken,
		&record.VerifiedAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("insert deployment domain: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("commit deployment domain: %w", err)
	}

	return record, nil
}

func (s *DeploymentStore) VerifyDeploymentDomain(ctx context.Context, userID string, deploymentID string, customDomain string) (DeploymentDomainRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentDomainRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	customDomain = normalizeCustomDomain(customDomain)
	if userID == "" || deploymentID == "" || customDomain == "" {
		return DeploymentDomainRecord{}, fmt.Errorf("userID, deploymentID, and customDomain are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return DeploymentDomainRecord{}, err
	}

	var record DeploymentDomainRecord
	if err := s.pool.QueryRow(ctx, `
SELECT id::text, deployment_id::text, project_id::text, custom_domain, verified, verification_token, verified_at, created_at, updated_at
FROM domains
WHERE deployment_id = $1 AND custom_domain = $2
`, deploymentID, customDomain).Scan(
		&record.ID,
		&record.DeploymentID,
		&record.ProjectID,
		&record.CustomDomain,
		&record.Verified,
		&record.VerificationToken,
		&record.VerifiedAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("load deployment domain: %w", err)
	}

	challengeName := verifyTXTChallengeName(record.CustomDomain)
	txtRecords, err := net.LookupTXT(challengeName)
	if err != nil {
		return record, fmt.Errorf("lookup txt record %s: %w", challengeName, err)
	}

	verified := false
	for _, candidate := range txtRecords {
		if strings.TrimSpace(candidate) == record.VerificationToken {
			verified = true
			break
		}
	}
	if !verified {
		return record, fmt.Errorf("verification token not found on %s", challengeName)
	}

	now := time.Now().UTC()
	if err := s.pool.QueryRow(ctx, `
UPDATE domains
SET verified = TRUE,
	verified_at = $3,
	updated_at = now()
WHERE id = $1
RETURNING id::text, deployment_id::text, project_id::text, custom_domain, verified, verification_token, verified_at, created_at, updated_at
`, record.ID, record.CustomDomain, now).Scan(
		&record.ID,
		&record.DeploymentID,
		&record.ProjectID,
		&record.CustomDomain,
		&record.Verified,
		&record.VerificationToken,
		&record.VerifiedAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("mark deployment domain verified: %w", err)
	}

	return record, nil
}

func (s *DeploymentStore) DeleteDeploymentDomain(ctx context.Context, userID string, deploymentID string, customDomain string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	customDomain = normalizeCustomDomain(customDomain)
	if userID == "" || deploymentID == "" || customDomain == "" {
		return fmt.Errorf("userID, deploymentID, and customDomain are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return err
	}

	_, err := s.pool.Exec(ctx, `
DELETE FROM domains
WHERE deployment_id = $1 AND custom_domain = $2
`, deploymentID, customDomain)
	if err != nil {
		return fmt.Errorf("delete deployment domain: %w", err)
	}

	return nil
}

func (s *DeploymentStore) ListDeploymentDomains(ctx context.Context, userID string, deploymentID string) ([]DeploymentDomainRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return nil, fmt.Errorf("userID and deploymentID are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, deployment_id::text, project_id::text, custom_domain, verified, verification_token, verified_at, created_at, updated_at
FROM domains
WHERE deployment_id = $1
ORDER BY custom_domain ASC
`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list deployment domains: %w", err)
	}
	defer rows.Close()

	domains := make([]DeploymentDomainRecord, 0)
	for rows.Next() {
		var record DeploymentDomainRecord
		if err := rows.Scan(
			&record.ID,
			&record.DeploymentID,
			&record.ProjectID,
			&record.CustomDomain,
			&record.Verified,
			&record.VerificationToken,
			&record.VerifiedAt,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan deployment domain: %w", err)
		}
		domains = append(domains, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployment domains: %w", err)
	}

	return domains, nil
}

func (s *DeploymentStore) ListVerifiedDomainsForDeployment(ctx context.Context, deploymentID string) ([]string, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return nil, fmt.Errorf("deploymentID is required")
	}

	rows, err := s.pool.Query(ctx, `
SELECT custom_domain
FROM domains
WHERE deployment_id = $1 AND verified = TRUE
ORDER BY custom_domain ASC
`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list verified deployment domains: %w", err)
	}
	defer rows.Close()

	domains := make([]string, 0)
	for rows.Next() {
		var customDomain string
		if err := rows.Scan(&customDomain); err != nil {
			return nil, fmt.Errorf("scan verified deployment domain: %w", err)
		}
		domains = append(domains, customDomain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verified deployment domains: %w", err)
	}

	return domains, nil
}

func (s *DeploymentStore) ListDomainsForDeploymentIDs(ctx context.Context, deploymentIDs []string) (map[string][]string, error) {
	result := make(map[string][]string, len(deploymentIDs))
	for _, deploymentID := range deploymentIDs {
		domains, err := s.ListVerifiedDomainsForDeployment(ctx, deploymentID)
		if err != nil {
			return nil, err
		}
		if len(domains) > 0 {
			result[deploymentID] = domains
		}
	}
	return result, nil
}

func sortDeploymentDomains(domains []DeploymentDomainRecord) {
	sort.Slice(domains, func(i, j int) bool {
		return domains[i].CustomDomain < domains[j].CustomDomain
	})
}
