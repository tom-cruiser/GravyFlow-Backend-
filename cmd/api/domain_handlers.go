package main

import (
    "context"
    "crypto/tls"
    "fmt"
    "log"
    "net"
    "net/http"
    "os"
    "regexp"
    "strconv"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
)

// ============================================================================
// TYPES
// ============================================================================

type DeploymentDomainRequest struct {
    CustomDomain string `json:"customDomain" binding:"required"`
}

type BulkDomainRequest struct {
    Domains []string `json:"domains" binding:"required"`
}

type VerificationConfig struct {
    MaxAttempts int
    RetryDelay  time.Duration
    Timeout     time.Duration
}

type DomainHealth struct {
    Domain       string    `json:"domain"`
    Status       string    `json:"status"`
    SSLValid     bool      `json:"sslValid"`
    SSLExpiry    time.Time `json:"sslExpiry"`
    ResponseTime int64     `json:"responseTime"`
    LastChecked  time.Time `json:"lastChecked"`
    Error        string    `json:"error,omitempty"`
}

type SSLInfo struct {
    Valid  bool
    Expiry time.Time
}

type RedirectConfig struct {
    FromDomain string `json:"fromDomain"`
    ToDomain   string `json:"toDomain"`
    Permanent  bool   `json:"permanent"`
}

// ============================================================================
// EXISTING HANDLERS (Enhanced)
// ============================================================================

func listAppDomainsHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    domains, err := deploymentStore.ListDeploymentDomains(
        c.Request.Context(), user.ID, deployment.DeploymentID,
    )
    if err != nil {
        lowerErr := strings.ToLower(err.Error())
        if strings.Contains(lowerErr, "not found") || strings.Contains(lowerErr, "deployment store is not initialized") {
            c.JSON(http.StatusNotFound, gin.H{
                "error":   "deployment_not_found",
                "details": err.Error(),
            })
            return
        }
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_list_domains",
            "details": err.Error(),
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "domains": domains,
        "count":   len(domains),
    })
}

func addAppDomainHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req DeploymentDomainRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    req.CustomDomain = normalizeCustomDomain(req.CustomDomain)
    if err := validateCustomDomainFormat(req.CustomDomain); err != nil {
        sendBadRequest(c, "invalid customDomain", err)
        return
    }

    // Check if auto-verify is requested
    autoVerify := c.DefaultQuery("autoVerify", "false") == "true"
    retryCount := 5
    if r := c.Query("retries"); r != "" {
        if val, err := strconv.Atoi(r); err == nil && val > 0 && val <= 10 {
            retryCount = val
        }
    }

    record, err := deploymentStore.UpsertDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, req.CustomDomain, nil,
    )
    if err != nil {
        lowerErr := strings.ToLower(err.Error())
        if strings.Contains(lowerErr, "not found") || strings.Contains(lowerErr, "deployment store is not initialized") {
            c.JSON(http.StatusNotFound, gin.H{
                "error":   "deployment_not_found",
                "details": err.Error(),
            })
            return
        }
        if strings.Contains(strings.ToLower(err.Error()), "already attached to another deployment") {
            c.JSON(http.StatusConflict, gin.H{
                "error":   "domain_already_in_use",
                "details": err.Error(),
            })
            return
        }
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_store_domain",
            "details": err.Error(),
        })
        return
    }

    // Attempt immediate verification if autoVerify is enabled
    if autoVerify {
        config := VerificationConfig{
            MaxAttempts: retryCount,
            RetryDelay:  10 * time.Second,
            Timeout:     time.Duration(retryCount*10) * time.Second,
        }

        ctx, cancel := context.WithTimeout(c.Request.Context(), config.Timeout)
        defer cancel()

        verified, err := autoVerifyDomain(ctx, req.CustomDomain, record.VerificationToken, config)
        if err == nil && verified {
            verifiedRecord, verifyErr := deploymentStore.VerifyDeploymentDomain(
                ctx, user.ID, deployment.DeploymentID, req.CustomDomain,
            )
            if verifyErr == nil {
                if syncErr := SyncCaddyRoutesFromRunningContainers(); syncErr != nil {
                    _ = deploymentStore.UpdateDomainStatus(ctx, verifiedRecord.ID, DomainStatusError, syncErr.Error())
                    c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "failed_to_sync_caddy",
                        "details": syncErr.Error(),
                    })
                    return
                }
                if verifiedRecord.Status == DomainStatusDNSVerified {
                    finalStatus := DomainStatusActive
                    finalMessage := "domain is live"
                    if caddyTLSEnabled() {
                        finalStatus = DomainStatusSSLProvisioning
                        finalMessage = "DNS verified; Caddy is obtaining a TLS certificate"
                    }
                    _ = deploymentStore.UpdateDomainStatus(ctx, verifiedRecord.ID, finalStatus, finalMessage)
                    verifiedRecord.Status = finalStatus
                    verifiedRecord.StatusMessage = finalMessage
                }
                c.JSON(http.StatusOK, gin.H{
                    "domain": verifiedRecord,
                    "status": "verified",
                    "method": "auto-verified",
                })
                return
            }
        }

        // Auto-verification failed, return challenge instructions
        challengeName := verifyTXTChallengeName(record.CustomDomain)
        challengeInstructions := fmt.Sprintf(
            "Auto-verification failed. Create a TXT record at %s with value %s, then call the verify endpoint.",
            challengeName, record.VerificationToken,
        )
        c.JSON(http.StatusAccepted, gin.H{
            "domain":      record,
            "status":      "pending",
            "challenge":   challengeName,
            "instruction": challengeInstructions,
            "autoAttempt": false,
        })
        return
    }

    // Manual verification flow
    challengeName := verifyTXTChallengeName(record.CustomDomain)
    challengeInstructions := fmt.Sprintf(
        "Create a TXT record at %s with value %s, then call the verify endpoint.",
        challengeName, record.VerificationToken,
    )
    c.JSON(http.StatusAccepted, gin.H{
        "domain":      record,
        "status":      "pending",
        "challenge":   challengeName,
        "instruction": challengeInstructions,
    })
}

func verifyAppDomainHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    customDomain := normalizeCustomDomain(c.Param("domain"))
    if customDomain == "" {
        sendBadRequest(c, "domain is required", nil)
        return
    }

    record, err := deploymentStore.VerifyDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, customDomain,
    )
    if err != nil {
        lowerErr := strings.ToLower(err.Error())
        if strings.Contains(lowerErr, "not found") {
            c.JSON(http.StatusNotFound, gin.H{
                "error":   "domain_not_found",
                "details": err.Error(),
            })
            return
        }
        c.JSON(http.StatusBadRequest, gin.H{
            "error":   "domain_not_verified",
            "details": err.Error(),
        })
        return
    }

    if syncErr := SyncCaddyRoutesFromRunningContainers(); syncErr != nil {
        // Ownership (and possibly reachability) already verified in the DB at
        // this point — record the sync failure as an explicit 'error' status
        // instead of leaving the domain looking verified with no indication
        // anything went wrong (the previous behavior: DB said verified,
        // Caddy never got the route, and the user had no way to tell).
        _ = deploymentStore.UpdateDomainStatus(c.Request.Context(), record.ID, DomainStatusError, syncErr.Error())
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_sync_caddy",
            "details": syncErr.Error(),
        })
        return
    }

    // Only advance past dns_verified once Caddy has actually accepted the
    // route — if checkDNSTarget (domains.go) found the domain isn't pointed
    // here yet, record.Status is still pending_dns and stays that way.
    if record.Status == DomainStatusDNSVerified {
        finalStatus := DomainStatusActive
        finalMessage := "domain is live"
        if caddyTLSEnabled() {
            finalStatus = DomainStatusSSLProvisioning
            finalMessage = "DNS verified; Caddy is obtaining a TLS certificate"
        }
        _ = deploymentStore.UpdateDomainStatus(c.Request.Context(), record.ID, finalStatus, finalMessage)
        record.Status = finalStatus
        record.StatusMessage = finalMessage
    }

    c.JSON(http.StatusOK, gin.H{
        "domain": record,
        "status": "verified",
    })
}

func deleteAppDomainHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    customDomain := normalizeCustomDomain(c.Param("domain"))
    if customDomain == "" {
        sendBadRequest(c, "domain is required", nil)
        return
    }

    if err := deploymentStore.DeleteDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, customDomain,
    ); err != nil {
        if strings.Contains(strings.ToLower(err.Error()), "not found") {
            c.JSON(http.StatusNotFound, gin.H{
                "error":   "domain_not_found",
                "details": err.Error(),
            })
            return
        }
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_delete_domain",
            "details": err.Error(),
        })
        return
    }

    // Clean up certificates and sync Caddy
    cleanupCaddyCertificatesForDomain(customDomain)
    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_sync_caddy",
            "details": err.Error(),
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{"message": "domain removed"})
}

// makePrimaryDomainHandler is the "Make Primary" toggle: exactly one domain
// per deployment is primary at a time (see DeploymentStore.MakeDomainPrimary).
func makePrimaryDomainHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    customDomain := normalizeCustomDomain(c.Param("domain"))
    if customDomain == "" {
        sendBadRequest(c, "domain is required", nil)
        return
    }

    record, err := deploymentStore.MakeDomainPrimary(c.Request.Context(), user.ID, deployment.DeploymentID, customDomain)
    if err != nil {
        if strings.Contains(strings.ToLower(err.Error()), "not found") {
            c.JSON(http.StatusNotFound, gin.H{"error": "domain_not_found", "details": err.Error()})
            return
        }
        c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_set_primary_domain", "details": err.Error()})
        return
    }

    // The primary domain drives the www<->apex redirect target, so a change
    // here can change what UpsertDomainRedirect should point at. Recomputing
    // that from the deployment's current edge settings keeps them in sync
    // without requiring the user to re-save Edge Settings after switching
    // primaries.
    if settings, err := deploymentStore.GetEdgeSettings(c.Request.Context(), user.ID, deployment.DeploymentID); err == nil {
        applyWWWRedirect(c.Request.Context(), user.ID, deployment.DeploymentID, record.CustomDomain, settings.WWWRedirectMode)
    }

    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_sync_caddy", "details": err.Error()})
        return
    }

    c.JSON(http.StatusOK, gin.H{"domain": record})
}

func getEdgeSettingsHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    settings, err := deploymentStore.GetEdgeSettings(c.Request.Context(), user.ID, deployment.DeploymentID)
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_edge_settings", "details": err.Error()})
        return
    }

    c.JSON(http.StatusOK, gin.H{"edgeSettings": settings})
}

// updateEdgeSettingsHandler controls Phase 4's per-service traffic rules:
// Force HTTPS and www<->apex redirect. Force HTTPS is read directly by
// caddy.go's RouteManager on every sync; the redirect is materialized as a
// domain_redirects row via the existing UpsertDomainRedirect (same mechanism
// addDomainRedirectHandler already uses), computed from whichever domain is
// currently primary.
func updateEdgeSettingsHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req EdgeSettings
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    settings, err := deploymentStore.UpdateEdgeSettings(c.Request.Context(), user.ID, deployment.DeploymentID, req)
    if err != nil {
        if strings.Contains(err.Error(), "wwwRedirectMode must be one of") {
            sendBadRequest(c, err.Error(), nil)
            return
        }
        c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_edge_settings", "details": err.Error()})
        return
    }

    domains, err := deploymentStore.ListDeploymentDomains(c.Request.Context(), user.ID, deployment.DeploymentID)
    if err == nil {
        for _, d := range domains {
            if d.IsPrimary {
                applyWWWRedirect(c.Request.Context(), user.ID, deployment.DeploymentID, d.CustomDomain, settings.WWWRedirectMode)
                break
            }
        }
    }

    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_sync_caddy", "details": err.Error()})
        return
    }

    c.JSON(http.StatusOK, gin.H{"edgeSettings": settings})
}

// applyWWWRedirect computes the from/to pair for a www<->apex redirect
// mode relative to primaryDomain and upserts it via the existing
// UpsertDomainRedirect. Best-effort: a failure here shouldn't fail the
// caller (edge settings / primary-domain changes still succeed), since the
// redirect is a secondary convenience, not the domain mapping itself.
func applyWWWRedirect(ctx context.Context, userID string, deploymentID string, primaryDomain string, mode string) {
    primaryDomain = normalizeCustomDomain(primaryDomain)
    if primaryDomain == "" {
        return
    }

    var from, to string
    switch mode {
    case "apex_to_www":
        from = primaryDomain
        to = "www." + primaryDomain
    case "www_to_apex":
        if !strings.HasPrefix(primaryDomain, "www.") {
            return
        }
        from = primaryDomain
        to = strings.TrimPrefix(primaryDomain, "www.")
    default:
        return
    }

    if err := deploymentStore.UpsertDomainRedirect(ctx, userID, deploymentID, from, to, true); err != nil {
        log.Printf("[WARN] applyWWWRedirect: failed to upsert redirect %s -> %s: %v", from, to, err)
    }
}

// ============================================================================
// NEW HANDLERS
// ============================================================================

func bulkAddAppDomainsHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req BulkDomainRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    if len(req.Domains) == 0 {
        sendBadRequest(c, "at least one domain is required", nil)
        return
    }

    if len(req.Domains) > 10 {
        c.JSON(http.StatusBadRequest, gin.H{"error": "maximum 10 domains per request"})
        return
    }

    results := make([]map[string]interface{}, 0, len(req.Domains))
    for _, domain := range req.Domains {
        domain = normalizeCustomDomain(domain)
        if err := validateCustomDomainFormat(domain); err != nil {
            results = append(results, map[string]interface{}{
                "domain": domain,
                "status": "error",
                "error":  err.Error(),
            })
            continue
        }

        record, err := deploymentStore.UpsertDeploymentDomain(
            c.Request.Context(), user.ID, deployment.DeploymentID, domain, nil,
        )
        if err != nil {
            results = append(results, map[string]interface{}{
                "domain": domain,
                "status": "error",
                "error":  err.Error(),
            })
            continue
        }

        results = append(results, map[string]interface{}{
            "domain": record,
            "status": "created",
        })
    }

    // Sync Caddy once after all additions
    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_sync_caddy",
            "details": err.Error(),
            "results": results,
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "results": results,
        "count":   len(results),
    })
}

func domainVerificationStatusHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    customDomain := normalizeCustomDomain(c.Param("domain"))
    if customDomain == "" {
        sendBadRequest(c, "domain is required", nil)
        return
    }

    // Get domain record
    record, err := deploymentStore.GetDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, customDomain,
    )
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "domain_not_found"})
        return
    }

    // Check verification status
    status := map[string]interface{}{
        "domain":   record.CustomDomain,
        "status":   map[bool]string{true: "verified", false: "pending"}[record.Verified],
        "verified": record.Verified,
    }

    if !record.Verified {
        // Check if DNS record exists
        exists, err := checkDNSRecord(c.Request.Context(), customDomain, record.VerificationToken)
        if err == nil && exists {
            status["dnsRecordFound"] = true
            status["readyToVerify"] = true
        } else {
            status["dnsRecordFound"] = false
            status["readyToVerify"] = false
            status["challenge"] = fmt.Sprintf("_acme-challenge.%s", customDomain)
            status["expectedValue"] = record.VerificationToken
            if err != nil {
                status["dnsError"] = err.Error()
            }
        }
    }

    if record.Verified {
        status["verifiedAt"] = record.VerifiedAt
        // Get health info
        health, err := checkDomainHealthQuick(customDomain)
        if err == nil {
            status["health"] = health
        }
    }

    c.JSON(http.StatusOK, status)
}

func checkDomainHealthHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    customDomain := normalizeCustomDomain(c.Param("domain"))
    if customDomain == "" {
        sendBadRequest(c, "domain is required", nil)
        return
    }

    // Get domain record
    record, err := deploymentStore.GetDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, customDomain,
    )
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "domain_not_found"})
        return
    }

    if !record.Verified {
        c.JSON(http.StatusBadRequest, gin.H{
            "error":  "domain_not_verified",
            "status": "pending",
        })
        return
    }

    health := DomainHealth{
        Domain:      customDomain,
        LastChecked: time.Now(),
    }

    // Check SSL certificate
    sslInfo, err := checkSSLCertificate(customDomain)
    if err != nil {
        health.Status = "error"
        health.Error = err.Error()
        c.JSON(http.StatusOK, health)
        return
    }
    health.SSLValid = sslInfo.Valid
    health.SSLExpiry = sslInfo.Expiry

    // Check response time
    client := &http.Client{
        Timeout: 5 * time.Second,
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            return http.ErrUseLastResponse
        },
    }

    start := time.Now()
    resp, err := client.Get(fmt.Sprintf("https://%s", customDomain))
    if err != nil {
        health.Status = "error"
        health.Error = err.Error()
    } else {
        defer resp.Body.Close()
        health.ResponseTime = time.Since(start).Milliseconds()
        if resp.StatusCode >= 200 && resp.StatusCode < 400 {
            health.Status = "healthy"
        } else {
            health.Status = "unhealthy"
            health.Error = fmt.Sprintf("HTTP status: %d", resp.StatusCode)
        }
    }

    c.JSON(http.StatusOK, health)
}

func addDomainRedirectHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req RedirectConfig
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    req.FromDomain = normalizeCustomDomain(req.FromDomain)
    req.ToDomain = normalizeCustomDomain(req.ToDomain)

    if err := validateCustomDomainFormat(req.FromDomain); err != nil {
        sendBadRequest(c, "invalid fromDomain", err)
        return
    }
    if err := validateCustomDomainFormat(req.ToDomain); err != nil {
        sendBadRequest(c, "invalid toDomain", err)
        return
    }

    if req.FromDomain == req.ToDomain {
        c.JSON(http.StatusBadRequest, gin.H{"error": "from and to domains must be different"})
        return
    }

    // Verify both domains belong to the deployment
    fromRecord, err := deploymentStore.GetDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, req.FromDomain,
    )
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "source_domain_not_found", "details": err.Error()})
        return
    }

    toRecord, err := deploymentStore.GetDeploymentDomain(
        c.Request.Context(), user.ID, deployment.DeploymentID, req.ToDomain,
    )
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "target_domain_not_found", "details": err.Error()})
        return
    }

    if !fromRecord.Verified || !toRecord.Verified {
        c.JSON(http.StatusBadRequest, gin.H{
            "error": "both domains must be verified",
        })
        return
    }

    // Store redirect in database
    err = deploymentStore.UpsertDomainRedirect(
        c.Request.Context(), user.ID, deployment.DeploymentID,
        req.FromDomain, req.ToDomain, req.Permanent,
    )
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_create_redirect",
            "details": err.Error(),
        })
        return
    }

    // Update Caddy configuration
    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_sync_caddy",
            "details": err.Error(),
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "message":   "redirect created",
        "from":      req.FromDomain,
        "to":        req.ToDomain,
        "permanent": req.Permanent,
    })
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

// dnsLookupTimeout bounds every DNS check in this file so a slow/unresponsive
// resolver can't hang a request goroutine indefinitely — the bare
// net.LookupTXT/net.LookupHost package functions used to have no timeout at
// all.
const dnsLookupTimeout = 5 * time.Second

func checkDNSRecord(ctx context.Context, domain string, expectedValue string) (bool, error) {
    ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
    defer cancel()

    challengeDomain := fmt.Sprintf("_acme-challenge.%s", domain)
    txts, err := net.DefaultResolver.LookupTXT(ctx, challengeDomain)
    if err != nil {
        return false, err
    }

    for _, txt := range txts {
        if txt == expectedValue {
            return true, nil
        }
    }
    return false, nil
}

// checkDNSTarget reports whether domain's CNAME/A record actually points at
// this platform yet — a signal separate from (and additional to) ownership
// verification in VerifyDeploymentDomain (domains.go). Ownership can be
// proven well before a user updates DNS to route real traffic here; without
// this, a "verified" domain could still be silently unreachable.
//
// GRAVYFLOW_PROXY_TARGET is the hostname or IP the platform's edge (Caddy)
// is reachable at — e.g. the value customers are told to CNAME to. Left
// unset in dev, where there's nothing public to point DNS at.
func checkDNSTarget(ctx context.Context, domain string) (bool, string) {
    proxyTarget := strings.TrimSpace(os.Getenv("GRAVYFLOW_PROXY_TARGET"))
    if proxyTarget == "" {
        return false, "DNS reachability can't be checked yet: GRAVYFLOW_PROXY_TARGET is not configured on this server"
    }

    ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
    defer cancel()

    if cname, err := net.DefaultResolver.LookupCNAME(ctx, domain); err == nil {
        if strings.TrimSuffix(strings.ToLower(cname), ".") == strings.TrimSuffix(strings.ToLower(proxyTarget), ".") {
            return true, fmt.Sprintf("CNAME correctly points to %s", proxyTarget)
        }
    }

    domainIPs, err := net.DefaultResolver.LookupHost(ctx, domain)
    if err != nil {
        return false, fmt.Sprintf("could not resolve %s: %v", domain, err)
    }
    targetIPs, err := net.DefaultResolver.LookupHost(ctx, proxyTarget)
    if err != nil {
        // GRAVYFLOW_PROXY_TARGET may already be a bare IP rather than a
        // resolvable hostname.
        targetIPs = []string{proxyTarget}
    }
    for _, d := range domainIPs {
        for _, t := range targetIPs {
            if d == t {
                return true, fmt.Sprintf("%s resolves to %s", domain, d)
            }
        }
    }
    return false, fmt.Sprintf(
        "%s does not currently point to this platform (resolves to %s, expected %s)",
        domain, strings.Join(domainIPs, ", "), proxyTarget,
    )
}

func autoVerifyDomain(ctx context.Context, domain string, token string, config VerificationConfig) (bool, error) {
    ticker := time.NewTicker(config.RetryDelay)
    defer ticker.Stop()

    timeout := time.After(config.Timeout)

    for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
        select {
        case <-timeout:
            return false, fmt.Errorf("verification timeout after %v", config.Timeout)
        case <-ticker.C:
            exists, err := checkDNSRecord(ctx, domain, token)
            if err != nil {
                // Log but continue
                log.Printf("DNS check error for %s: %v", domain, err)
                continue
            }
            if exists {
                return true, nil
            }
            log.Printf("DNS record not found for %s (attempt %d/%d)", domain, attempt, config.MaxAttempts)
        }
    }

    return false, nil
}

func checkSSLCertificate(domain string) (SSLInfo, error) {
    conn, err := tls.Dial("tcp", fmt.Sprintf("%s:443", domain), &tls.Config{
        InsecureSkipVerify: true,
    })
    if err != nil {
        return SSLInfo{}, err
    }
    defer conn.Close()

    certs := conn.ConnectionState().PeerCertificates
    if len(certs) == 0 {
        return SSLInfo{Valid: false}, fmt.Errorf("no certificates found")
    }

    cert := certs[0]
    now := time.Now()
    return SSLInfo{
        Valid:  now.After(cert.NotBefore) && now.Before(cert.NotAfter),
        Expiry: cert.NotAfter,
    }, nil
}

func checkDomainHealthQuick(domain string) (map[string]interface{}, error) {
    client := &http.Client{
        Timeout: 3 * time.Second,
    }

    start := time.Now()
    resp, err := client.Get(fmt.Sprintf("https://%s", domain))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    return map[string]interface{}{
        "responseTime": time.Since(start).Milliseconds(),
        "statusCode":   resp.StatusCode,
    }, nil
}

func verifyTXTChallengeName(domain string) string {
    return fmt.Sprintf("_acme-challenge.%s", domain)
}

// hostnameLabelPattern matches a single DNS label: 1-63 chars, alphanumeric,
// hyphens allowed in the middle but not at either end. Underscores are
// deliberately excluded even though isValidHostName (caddy.go) allows them
// for internal container-derived hosts — a user-submitted custom domain
// should look like a real, registrable DNS name.
var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validateCustomDomainFormat rejects the input shapes that
// normalizeCustomDomain's lowercase+trim alone lets through untouched:
// bare IP literals, wildcards, single-label names (e.g. "localhost"),
// empty/oversized labels, and any character outside [a-z0-9.-]. Without
// this, a string like "169.254.169.254" or "internal-service" was accepted
// as a "custom domain" and later dialed directly by checkDomainHealthHandler/
// checkSSLCertificate/checkDomainHealthQuick.
func validateCustomDomainFormat(domain string) error {
    if domain == "" {
        return fmt.Errorf("domain is required")
    }
    if len(domain) > 253 {
        return fmt.Errorf("domain is too long")
    }
    if strings.Contains(domain, "*") {
        return fmt.Errorf("wildcard domains are not supported")
    }
    if net.ParseIP(domain) != nil {
        return fmt.Errorf("domain must be a hostname, not an IP address")
    }

    labels := strings.Split(domain, ".")
    if len(labels) < 2 {
        return fmt.Errorf("domain must have at least two labels (e.g. example.com)")
    }
    for _, label := range labels {
        if len(label) == 0 || len(label) > 63 || !hostnameLabelPattern.MatchString(label) {
            return fmt.Errorf("domain contains an invalid label %q", label)
        }
    }
    return nil
}

// Route registration for all of these handlers lives in main.go's
// setupRouter, under /api/v1/apps/:id/domains/... (the prefix the frontend
// actually calls). This file used to also define a SetupDomainRoutes that
// registered the same handlers again under /api/deployments/:id/domains/...,
// but that function was never called from main.go, so bulkAddAppDomainsHandler,
// domainVerificationStatusHandler, checkDomainHealthHandler and
// addDomainRedirectHandler were unreachable 404s despite being fully
// implemented. It was removed rather than wired up, to avoid two divergent
// URL namespaces for the same feature.