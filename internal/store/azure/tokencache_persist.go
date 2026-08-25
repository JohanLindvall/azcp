//go:build linux || windows || (darwin && cgo)

package azure

import (
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity/cache"
)

// The persistent cache keeps tokens in whatever secure store the platform
// provides, so a sign-in outlives the process. macOS reaches its keychain
// through cgo, which a binary cross-compiled from another platform cannot do;
// the build tag above keeps such a build working, at the cost of signing in
// once per run. See tokencache_memory.go.

// cacheName isolates this tool's cached tokens from other applications'.
const cacheName = "azcp"

// tokenCache returns the shared persistent cache, or the zero value when the
// platform cannot provide one.
func (c *Credentials) tokenCache() azidentity.Cache {
	c.cacheOnce.Do(func() {
		tc, err := cache.New(&cache.Options{Name: cacheName})
		if err != nil {
			c.logger().Debug("no persistent token cache on this system, "+
				"so a sign-in will not outlive this run", "error", err)
			return
		}
		c.cache = tc
		c.persistent = true
	})
	return c.cache
}
