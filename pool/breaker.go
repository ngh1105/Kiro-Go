package pool

import (
	"sync"
	"time"
)

// BreakerState represents the state of a circuit breaker.
type BreakerState int

const (
	StateClosed   BreakerState = iota
	StateOpen
	StateHalfOpen
	StateDisabled
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	case StateDisabled:
		return "DISABLED"
	default:
		return "UNKNOWN"
	}
}

// CircuitBreaker implements a 3-state circuit breaker pattern (CLOSED → OPEN → HALF_OPEN).
//
//	CLOSED ──(N consecutive errors)──→ OPEN
//	  ↑                                  │
//	  │                          (timeout T)
//	  │                                  ↓
//	  └────────(1 success)─────── HALF_OPEN
//	                                  │
//	                          (1 failure) → OPEN
//
// Auth failures transition directly to DISABLED (terminal).
type CircuitBreaker struct {
	mu               sync.Mutex
	state            BreakerState
	errorCount       int
	errorThreshold   int
	openAt           time.Time
	transientTimeout time.Duration
	quotaTimeout     time.Duration
	authTimeout      time.Duration
	openTimeout      time.Duration // effective timeout for current open period
}

// NewCircuitBreaker creates a circuit breaker with per-error-type timeouts.
//
//	transientTimeout: how long the breaker stays OPEN after transient errors (e.g., 30s)
//	quotaTimeout:     how long the breaker stays OPEN after quota errors (e.g., 300s)
//	authTimeout:      timeout for auth failures — auth failures go directly to DISABLED
func NewCircuitBreaker(errorThreshold int, transientTimeout, quotaTimeout, authTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		errorThreshold:   errorThreshold,
		transientTimeout: transientTimeout,
		quotaTimeout:     quotaTimeout,
		authTimeout:      authTimeout,
	}
}

// Transition records the result of a request attempt.
// err == nil → success (resets to CLOSED).
// err != nil → failure: errorType "auth" → DISABLED, "quota" → quota timeout, else transient.
func (b *CircuitBreaker) Transition(err error, errorType string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateDisabled {
		return
	}

	if err == nil {
		b.errorCount = 0
		b.state = StateClosed
		return
	}

	b.errorCount++

	switch errorType {
	case "auth":
		b.state = StateDisabled
		return
	case "quota":
		b.openTimeout = b.quotaTimeout
	default:
		b.openTimeout = b.transientTimeout
	}

	if b.state == StateHalfOpen {
		b.state = StateOpen
		b.openAt = time.Now()
		return
	}

	if b.errorCount >= b.errorThreshold {
		b.state = StateOpen
		b.openAt = time.Now()
	}
}

// CanRoute reports whether requests may be routed through this breaker.
// OPEN breakers become HALF_OPEN once the timeout expires.
func (b *CircuitBreaker) CanRoute() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateDisabled:
		return false
	case StateOpen:
		if time.Since(b.openAt) >= b.openTimeout {
			b.state = StateHalfOpen
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return false
	}
}

// StateString returns the current state as a string.
func (b *CircuitBreaker) StateString() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}

// Reset forces the breaker back to CLOSED (e.g., manual operator action).
func (b *CircuitBreaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.errorCount = 0
}
