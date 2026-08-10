package main

import (
    "context"
    "crypto/tls"
    "fmt"
    "log"
    "net"
    "net/http"
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
    if req.CustomDomain == "" {
        sendBadRequest(c, "customDomain is required", nil)
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
                    c.JSON(http.StatusInternalServerError, gin.H{
                        "error":   "failed_to_sync_caddy",
                        "details": syncErr.Error(),
                    })
                    return
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
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_sync_caddy",
            "details": syncErr.Error(),
        })
        return
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
        if domain == "" {
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
        exists, err := checkDNSRecord(customDomain, record.VerificationToken)
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

    if req.FromDomain == "" || req.ToDomain == "" {
        sendBadRequest(c, "both fromDomain and toDomain are required", nil)
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

func checkDNSRecord(domain string, expectedValue string) (bool, error) {
    // Query DNS TXT record
    challengeDomain := fmt.Sprintf("_acme-challenge.%s", domain)
    txts, err := net.LookupTXT(challengeDomain)
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

func autoVerifyDomain(ctx context.Context, domain string, token string, config VerificationConfig) (bool, error) {
    ticker := time.NewTicker(config.RetryDelay)
    defer ticker.Stop()

    timeout := time.After(config.Timeout)

    for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
        select {
        case <-timeout:
            return false, fmt.Errorf("verification timeout after %v", config.Timeout)
        case <-ticker.C:
            exists, err := checkDNSRecord(domain, token)
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

// ============================================================================
// ROUTE SETUP
// ============================================================================

func SetupDomainRoutes(router *gin.Engine) {
    domainGroup := router.Group("/api/deployments/:id/domains")
    domainGroup.Use(AuthMiddleware(false))
    {
        // List domains
        domainGroup.GET("/", listAppDomainsHandler)
        
        // Add domains
        domainGroup.POST("/", addAppDomainHandler)
        domainGroup.POST("/bulk", bulkAddAppDomainsHandler)
        
        // Domain verification
        domainGroup.GET("/:domain/status", domainVerificationStatusHandler)
        domainGroup.POST("/:domain/verify", verifyAppDomainHandler)
        
        // Domain health
        domainGroup.GET("/:domain/health", checkDomainHealthHandler)
        
        // Domain redirects
        domainGroup.POST("/redirects", addDomainRedirectHandler)
        
        // Delete domain
        domainGroup.DELETE("/:domain", deleteAppDomainHandler)
    }
}