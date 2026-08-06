package main

// ============================================================================
// GLOBAL VARIABLES
// ============================================================================

var (
	// deploymentStore is the global database store instance
	deploymentStore *DeploymentStore
	
	// deploymentJobs is the global job manager instance
	deploymentJobs *DeploymentJobManager
	
	// defaultRouteManager is the global route manager for Caddy
	defaultRouteManager *RouteManager
)