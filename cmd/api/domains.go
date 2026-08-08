package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ============================================================================
// TYPES
// ============================================================================

type DeploymentDomainRecord struct {
	ID                string     `json:"id"`
	DeploymentID      string     `json:"deploymentId"`
	ProjectID         string     `json:"projectId"`
	CustomDomain      string     `json:"customDomain"`
	Verified          bool       `json:"verified"`
	VerificationToken string     `json:"verificationToken,omitempty"`
	VerifiedAt        *time.Time `json:"verifiedAt,omitempty"`
	ExpiresAt         *time.Time `json:"expiresAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type DomainVerificationHistory struct {
	ID        string    `json:"id"`
	DomainID  string    `json:"domainId"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type SSLCertificate struct {
	ID          string    `json:"id"`
	DomainID    string    `json:"domainId"`
	Issuer      string    `json:"issuer"`
	ValidFrom   time.Time `json:"validFrom"`
	ValidTo     time.Time `json:"validTo"`
	Serial      string    `json:"serial"`
	Status      string    `json:"status"`
	LastChecked time.Time `json:"lastChecked"`
	CreatedAt   time.Time `json:"createdAt"`
}

type DomainAnalytics struct {
	DomainID        string    `json:"domainId"`
	TotalRequests   int64     `json:"totalRequests"`
	UniqueVisitors  int64     `json:"uniqueVisitors"`
	AvgResponseTime float64   `json:"avgResponseTime"`
	ErrorRate       float64   `json:"errorRate"`
	LastActivity    time.Time `json:"lastActivity"`
}

type DomainFilter struct {
	Search        string
	Verified      *bool
	CreatedAfter  time.Time
	CreatedBefore time.Time
	Limit         int
	Offset        int
}

// ============================================================================
// NOTE: normalizeCustomDomain and verifyTXTChallengeName are now in caddy.go
// DO NOT redeclare them here
// ============================================================================

// ============================================================================
// ENHANCED UPSERT WITH EXPIRY
// ============================================================================

func (s *DeploymentStore) UpsertDeploymentDomain(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
	expiresAt *time.Time,
) (DeploymentDomainRecord, error) {
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
			expires_at = $3,
			updated_at = now()
		WHERE custom_domain = $1
		RETURNING id::text, deployment_id::text, project_id::text, custom_domain, 
				  verified, verification_token, verified_at, expires_at, 
				  created_at, updated_at
		`, customDomain, verificationToken, expiresAt).Scan(
			&record.ID,
			&record.DeploymentID,
			&record.ProjectID,
			&record.CustomDomain,
			&record.Verified,
			&record.VerificationToken,
			&record.VerifiedAt,
			&record.ExpiresAt,
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
		verified_at,
		expires_at
	) VALUES (
		(SELECT project_id FROM deployments WHERE id = $1),
		$1,
		$1,
		$2,
		FALSE,
		$3,
		NULL,
		$4
	)
	RETURNING id::text, deployment_id::text, project_id::text, custom_domain, 
			  verified, verification_token, verified_at, expires_at, 
			  created_at, updated_at
	`, deploymentID, customDomain, verificationToken, expiresAt).Scan(
		&record.ID,
		&record.DeploymentID,
		&record.ProjectID,
		&record.CustomDomain,
		&record.Verified,
		&record.VerificationToken,
		&record.VerifiedAt,
		&record.ExpiresAt,
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

func (s *DeploymentStore) UpsertDomainRedirect(
	ctx context.Context,
	userID string,
	deploymentID string,
	fromDomain string,
	toDomain string,
	permanent bool,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	fromDomain = normalizeCustomDomain(fromDomain)
	toDomain = normalizeCustomDomain(toDomain)
	if userID == "" || deploymentID == "" || fromDomain == "" || toDomain == "" {
		return fmt.Errorf("userID, deploymentID, fromDomain, and toDomain are required")
	}

	deployment, err := s.GetDeploymentForUser(ctx, userID, deploymentID)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO domain_redirects (
	project_id,
	deployment_id,
	from_domain,
	to_domain,
	permanent,
	updated_at
) VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (from_domain) DO UPDATE SET
	project_id = EXCLUDED.project_id,
	deployment_id = EXCLUDED.deployment_id,
	to_domain = EXCLUDED.to_domain,
	permanent = EXCLUDED.permanent,
	updated_at = now()
`, deployment.ProjectID, deploymentID, fromDomain, toDomain, permanent)
	if err != nil {
		return fmt.Errorf("upsert domain redirect: %w", err)
	}

	return nil
}

// ============================================================================
// ENHANCED VERIFICATION WITH HISTORY
// ============================================================================

func (s *DeploymentStore) VerifyDeploymentDomain(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
) (DeploymentDomainRecord, error) {
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
	SELECT id::text, deployment_id::text, project_id::text, custom_domain, 
		   verified, verification_token, verified_at, expires_at,
		   created_at, updated_at
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
		&record.ExpiresAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("load deployment domain: %w", err)
	}

	challengeName := verifyTXTChallengeName(record.CustomDomain)
	txtRecords, err := net.LookupTXT(challengeName)
	if err != nil {
		// Record failed attempt
		_ = s.RecordDomainVerificationAttempt(ctx, record.ID, "failed", err.Error())
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
		_ = s.RecordDomainVerificationAttempt(ctx, record.ID, "failed", "verification token not found")
		return record, fmt.Errorf("verification token not found on %s", challengeName)
	}

	now := time.Now().UTC()
	// Set expiry to 1 year from now if not set
	expiresAt := record.ExpiresAt
	if expiresAt == nil {
		oneYear := now.AddDate(1, 0, 0)
		expiresAt = &oneYear
	}

	if err := s.pool.QueryRow(ctx, `
	UPDATE domains
	SET verified = TRUE,
		verified_at = $3,
		expires_at = $4,
		updated_at = now()
	WHERE id = $1
	RETURNING id::text, deployment_id::text, project_id::text, custom_domain,
			  verified, verification_token, verified_at, expires_at,
			  created_at, updated_at
	`, record.ID, record.CustomDomain, now, expiresAt).Scan(
		&record.ID,
		&record.DeploymentID,
		&record.ProjectID,
		&record.CustomDomain,
		&record.Verified,
		&record.VerificationToken,
		&record.VerifiedAt,
		&record.ExpiresAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return DeploymentDomainRecord{}, fmt.Errorf("mark deployment domain verified: %w", err)
	}

	// Record successful verification
	_ = s.RecordDomainVerificationAttempt(ctx, record.ID, "succeeded", "domain verified successfully")

	return record, nil
}

// ============================================================================
// DOMAIN HISTORY
// ============================================================================

func (s *DeploymentStore) RecordDomainVerificationAttempt(
	ctx context.Context,
	domainID string,
	status string,
	message string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	_, err := s.pool.Exec(ctx, `
	INSERT INTO domain_verification_history (domain_id, status, message)
	VALUES ($1, $2, $3)
	`, domainID, status, message)
	return err
}

func (s *DeploymentStore) GetDomainVerificationHistory(
	ctx context.Context,
	domainID string,
	limit int,
) ([]DomainVerificationHistory, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	if limit == 0 || limit > 50 {
		limit = 10
	}

	rows, err := s.pool.Query(ctx, `
	SELECT id::text, domain_id::text, status, message, created_at
	FROM domain_verification_history
	WHERE domain_id = $1
	ORDER BY created_at DESC
	LIMIT $2
	`, domainID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []DomainVerificationHistory
	for rows.Next() {
		var record DomainVerificationHistory
		if err := rows.Scan(
			&record.ID,
			&record.DomainID,
			&record.Status,
			&record.Message,
			&record.CreatedAt,
		); err != nil {
			return nil, err
		}
		history = append(history, record)
	}
	return history, nil
}

// ============================================================================
// SSL CERTIFICATE TRACKING
// ============================================================================

func (s *DeploymentStore) UpdateSSLCertificate(
	ctx context.Context,
	domainID string,
	issuer string,
	validFrom time.Time,
	validTo time.Time,
	serial string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	_, err := s.pool.Exec(ctx, `
	INSERT INTO ssl_certificates (domain_id, issuer, valid_from, valid_to, serial, status)
	VALUES ($1, $2, $3, $4, $5, 'active')
	ON CONFLICT (domain_id) DO UPDATE SET
		issuer = EXCLUDED.issuer,
		valid_from = EXCLUDED.valid_from,
		valid_to = EXCLUDED.valid_to,
		serial = EXCLUDED.serial,
		status = 'active',
		last_checked = now(),
		updated_at = now()
	`, domainID, issuer, validFrom, validTo, serial)
	return err
}

func (s *DeploymentStore) GetExpiringDomains(
	ctx context.Context,
	daysThreshold int,
) ([]DeploymentDomainRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	threshold := time.Now().Add(time.Duration(daysThreshold) * 24 * time.Hour)

	rows, err := s.pool.Query(ctx, `
	SELECT d.id::text, d.deployment_id::text, d.project_id::text, d.custom_domain,
		   d.verified, d.verification_token, d.verified_at, d.created_at, d.updated_at
	FROM domains d
	INNER JOIN ssl_certificates s ON s.domain_id = d.id
	WHERE s.valid_to < $1 AND d.verified = TRUE
	ORDER BY s.valid_to ASC
	`, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []DeploymentDomainRecord
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
			return nil, err
		}
		domains = append(domains, record)
	}
	return domains, nil
}

func (s *DeploymentStore) RenewDomainVerification(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	record, err := s.GetDeploymentDomain(ctx, userID, deploymentID, customDomain)
	if err != nil {
		return err
	}

	// Check if domain is close to expiry
	if record.ExpiresAt != nil && time.Until(*record.ExpiresAt) > 7*24*time.Hour {
		return fmt.Errorf("domain not due for renewal")
	}

	// Generate new token and reset verification
	newToken, err := generateRandomToken(12)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
	UPDATE domains
	SET verified = FALSE,
		verification_token = $1,
		verified_at = NULL,
		updated_at = now()
	WHERE id = $2 AND deployment_id = $3
	`, newToken, record.ID, deploymentID)

	return err
}

// ============================================================================
// DOMAIN ANALYTICS
// ============================================================================

func (s *DeploymentStore) UpdateDomainAnalytics(
	ctx context.Context,
	domainID string,
	requestCount int,
	responseTime float64,
	isError bool,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	_, err := s.pool.Exec(ctx, `
	INSERT INTO domain_analytics (domain_id, requests, avg_response_time, error_count, last_activity)
	VALUES ($1, $2, $3, $4, now())
	ON CONFLICT (domain_id) DO UPDATE SET
		requests = domain_analytics.requests + EXCLUDED.requests,
		avg_response_time = (domain_analytics.avg_response_time + EXCLUDED.avg_response_time) / 2,
		error_count = domain_analytics.error_count + EXCLUDED.error_count,
		last_activity = EXCLUDED.last_activity
	`, domainID, requestCount, responseTime, boolToInt(isError))
	return err
}

func (s *DeploymentStore) GetDomainAnalytics(
	ctx context.Context,
	domainID string,
) (DomainAnalytics, error) {
	if s == nil || s.pool == nil {
		return DomainAnalytics{}, fmt.Errorf("deployment store is not initialized")
	}

	var analytics DomainAnalytics
	err := s.pool.QueryRow(ctx, `
	SELECT domain_id::text, requests, avg_response_time, error_count, last_activity
	FROM domain_analytics
	WHERE domain_id = $1
	`, domainID).Scan(
		&analytics.DomainID,
		&analytics.TotalRequests,
		&analytics.AvgResponseTime,
		&analytics.ErrorRate,
		&analytics.LastActivity,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DomainAnalytics{}, nil
		}
		return DomainAnalytics{}, err
	}
	// Calculate error rate as percentage
	if analytics.TotalRequests > 0 {
		analytics.ErrorRate = (analytics.ErrorRate / float64(analytics.TotalRequests)) * 100
	}
	return analytics, nil
}

// ============================================================================
// DOMAIN SEARCH
// ============================================================================

func (s *DeploymentStore) SearchDomains(
	ctx context.Context,
	userID string,
	filter DomainFilter,
) ([]DeploymentDomainRecord, int, error) {
	if s == nil || s.pool == nil {
		return nil, 0, fmt.Errorf("deployment store is not initialized")
	}

	if filter.Limit == 0 {
		filter.Limit = 20
	}
	if filter.Limit > 100 {
		filter.Limit = 100
	}

	// Build query conditions
	conditions := []string{"d.deployment_id IN (SELECT id FROM deployments WHERE owner_user_id = $1)"}
	args := []interface{}{userID}
	argIndex := 2

	if filter.Search != "" {
		conditions = append(conditions, fmt.Sprintf("d.custom_domain ILIKE $%d", argIndex))
		args = append(args, "%"+filter.Search+"%")
		argIndex++
	}

	if filter.Verified != nil {
		conditions = append(conditions, fmt.Sprintf("d.verified = $%d", argIndex))
		args = append(args, *filter.Verified)
		argIndex++
	}

	if !filter.CreatedAfter.IsZero() {
		conditions = append(conditions, fmt.Sprintf("d.created_at >= $%d", argIndex))
		args = append(args, filter.CreatedAfter)
		argIndex++
	}

	if !filter.CreatedBefore.IsZero() {
		conditions = append(conditions, fmt.Sprintf("d.created_at <= $%d", argIndex))
		args = append(args, filter.CreatedBefore)
		argIndex++
	}

	whereClause := strings.Join(conditions, " AND ")

	// Get total count
	var total int
	countQuery := fmt.Sprintf(`
	SELECT COUNT(*) FROM domains d WHERE %s
	`, whereClause)

	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Get paginated results
	query := fmt.Sprintf(`
	SELECT d.id::text, d.deployment_id::text, d.project_id::text, d.custom_domain,
		   d.verified, d.verification_token, d.verified_at, d.created_at, d.updated_at
	FROM domains d
	WHERE %s
	ORDER BY d.created_at DESC
	LIMIT $%d OFFSET $%d
	`, whereClause, argIndex, argIndex+1)

	args = append(args, filter.Limit, filter.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var domains []DeploymentDomainRecord
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
			return nil, 0, err
		}
		domains = append(domains, record)
	}

	return domains, total, nil
}

// ============================================================================
// DOMAIN TRANSFER
// ============================================================================

func (s *DeploymentStore) TransferDomain(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
	newDeploymentID string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	// Verify user owns both deployments
	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return fmt.Errorf("source deployment not found: %w", err)
	}
	if _, err := s.GetDeploymentForUser(ctx, userID, newDeploymentID); err != nil {
		return fmt.Errorf("target deployment not found: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Lock and verify domain
	var currentDeploymentID string
	if err := tx.QueryRow(ctx, `
	SELECT deployment_id::text FROM domains
	WHERE custom_domain = $1
	FOR UPDATE
	`, customDomain).Scan(&currentDeploymentID); err != nil {
		return err
	}

	if currentDeploymentID != deploymentID {
		return fmt.Errorf("domain not owned by source deployment")
	}

	// Generate new token for re-verification
	newToken, err := generateRandomToken(12)
	if err != nil {
		return err
	}

	// Transfer domain
	_, err = tx.Exec(ctx, `
	UPDATE domains
	SET deployment_id = $1,
		project_id = (SELECT project_id FROM deployments WHERE id = $1),
		verified = FALSE,
		verification_token = $2,
		verified_at = NULL,
		updated_at = now()
	WHERE custom_domain = $3
	`, newDeploymentID, newToken, customDomain)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ============================================================================
// GET SINGLE DOMAIN
// ============================================================================

func (s *DeploymentStore) GetDeploymentDomain(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
) (DeploymentDomainRecord, error) {
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
	err := s.pool.QueryRow(ctx, `
	SELECT id::text, deployment_id::text, project_id::text, custom_domain,
		   verified, verification_token, verified_at, expires_at,
		   created_at, updated_at
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
		&record.ExpiresAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeploymentDomainRecord{}, fmt.Errorf("domain not found")
		}
		return DeploymentDomainRecord{}, err
	}

	return record, nil
}

// ============================================================================
// EXISTING FUNCTIONS (Unchanged)
// ============================================================================

func (s *DeploymentStore) DeleteDeploymentDomain(
	ctx context.Context,
	userID string,
	deploymentID string,
	customDomain string,
) error {
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

	// Also delete associated history and analytics
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Get domain ID first
	var domainID string
	if err := tx.QueryRow(ctx, `
	SELECT id::text FROM domains
	WHERE deployment_id = $1 AND custom_domain = $2
	`, deploymentID, customDomain).Scan(&domainID); err != nil {
		return err
	}

	// Delete history
	if _, err := tx.Exec(ctx, `
	DELETE FROM domain_verification_history WHERE domain_id = $1
	`, domainID); err != nil {
		return err
	}

	// Delete SSL certificate
	if _, err := tx.Exec(ctx, `
	DELETE FROM ssl_certificates WHERE domain_id = $1
	`, domainID); err != nil {
		return err
	}

	// Delete analytics
	if _, err := tx.Exec(ctx, `
	DELETE FROM domain_analytics WHERE domain_id = $1
	`, domainID); err != nil {
		return err
	}

	// Delete domain
	if _, err := tx.Exec(ctx, `
	DELETE FROM domains WHERE id = $1
	`, domainID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *DeploymentStore) ListDeploymentDomains(
	ctx context.Context,
	userID string,
	deploymentID string,
) ([]DeploymentDomainRecord, error) {
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
	SELECT id::text, deployment_id::text, project_id::text, custom_domain,
		   verified, verification_token, verified_at, expires_at,
		   created_at, updated_at
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
			&record.ExpiresAt,
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

func (s *DeploymentStore) ListVerifiedDomainsForDeployment(
	ctx context.Context,
	deploymentID string,
) ([]string, error) {
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

func (s *DeploymentStore) ListDomainsForDeploymentIDs(
	ctx context.Context,
	deploymentIDs []string,
) (map[string][]string, error) {
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

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func sortDeploymentDomains(domains []DeploymentDomainRecord) {
	sort.Slice(domains, func(i, j int) bool {
		return domains[i].CustomDomain < domains[j].CustomDomain
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}