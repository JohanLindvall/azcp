//go:build !(linux || windows || (darwin && cgo))

package azure

import "github.com/Azure/azure-sdk-for-go/sdk/azidentity"

// This build has no secure store to keep tokens in — most often a macOS binary
// cross-compiled without cgo, which cannot reach the keychain. Tokens are held
// in memory instead, so a sign-in lasts for the run and no longer. Building on
// the target platform restores the persistent cache.

// tokenCache returns the zero value, which means the credential caches tokens
// in memory only.
func (c *Credentials) tokenCache() azidentity.Cache {
	c.cacheOnce.Do(func() {
		c.logger().Debug("this build has no persistent token cache, " +
			"so a sign-in will not outlive this run")
	})
	return c.cache
}
