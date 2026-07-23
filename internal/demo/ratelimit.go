package demo

import (
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter is a token bucket per client IP, sized per method class: reads
// poll every few seconds, writes are button presses.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
}

func NewRateLimiter(perTenSeconds int) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perTenSeconds) / 10,
		burst:   float64(perTenSeconds),
	}
}

func (r *RateLimiter) allow(ip string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[ip]
	if !ok {
		b = &bucket{tokens: r.burst}
		r.buckets[ip] = b
		if len(r.buckets) > 10000 {
			for k, v := range r.buckets {
				if now.Sub(v.lastSeen) > 10*time.Minute {
					delete(r.buckets, k)
				}
			}
		}
	} else {
		b.tokens += now.Sub(b.lastSeen).Seconds() * r.rate
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP prefers CF-Connecting-IP: behind the Cloudflare tunnel that header
// is set by Cloudflare itself, while X-Forwarded-For (what RealIP reads) can
// be seeded by the client to hop between buckets.
func clientIP(c echo.Context) string {
	if ip := c.Request().Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	return c.RealIP()
}

func (r *RateLimiter) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !r.allow(clientIP(c), time.Now()) {
				c.Response().Header().Set("Retry-After", "10")
				return c.JSON(http.StatusTooManyRequests, map[string]any{"ok": false, "error": "rate limited"})
			}
			return next(c)
		}
	}
}
