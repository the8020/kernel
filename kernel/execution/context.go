// Package execution owns context shared across the generic execution system.
package execution

import (
	"context"
	"errors"
	"fmt"

	"the8020/kernel/sandbox/model"
)

type callerKey struct{}

const SystemUsername = "system"

var ErrInvalidUser = errors.New("execution user is unavailable")

type User struct {
	ID       string `json:"userId"`
	Username string `json:"username"`
}

func UserForUsername(username string) (User, error) {
	if len(username) < 3 || len(username) > 32 {
		return User{}, invalidUsername("must be between 3 and 32 characters")
	}
	for _, character := range username {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return User{}, invalidUsername("must contain only lowercase letters and digits")
		}
	}
	return User{ID: "user:" + username, Username: username}, nil
}

func ValidateUsername(username string) error {
	_, err := UserForUsername(username)
	return err
}

func invalidUsername(message string) error {
	return fmt.Errorf("invalid execution username: %s", message)
}

func SystemUser() User {
	user, _ := UserForUsername(SystemUsername)
	return user
}

// DefaultUser assigns kernel-originated work to system and nested work to its caller.
// Principals are structural identities independent of application account rows.
func DefaultUser(ctx context.Context) User {
	if caller, ok := CallerFromContext(ctx); ok {
		return caller.User
	}
	return SystemUser()
}

func (u User) Valid() bool {
	validated, err := UserForUsername(u.Username)
	return err == nil && u == validated
}

func (u User) Empty() bool { return u == (User{}) }

type OriginType string

const (
	OriginService OriginType = "service"
	OriginJob     OriginType = "job"
	OriginProgram OriginType = "program"
)

func (t OriginType) Valid() bool {
	return t == OriginService || t == OriginJob || t == OriginProgram
}

type Origin struct {
	Type OriginType `json:"type"`
	ID   string     `json:"id"`
}

func (o Origin) Valid() bool { return o.Type.Valid() && o.ID != "" }

func (o Origin) ValidForWorkload(workload model.WorkloadType) bool {
	if !o.Valid() {
		return false
	}
	return workload == model.WorkloadService && o.Type == OriginService ||
		workload == model.WorkloadJob && (o.Type == OriginJob || o.Type == OriginProgram)
}

type Caller struct {
	ExecutionID string
	Workload    model.WorkloadType
	User        User
}

// WithCaller records the validated runtime execution making a synchronous
// kernel call. Schedulers use it to avoid making child work queue behind its
// waiting parent.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	if ctx == nil || caller.ExecutionID == "" || !caller.Workload.Valid() || !caller.User.Valid() {
		return ctx
	}
	return context.WithValue(ctx, callerKey{}, caller)
}

func CallerFromContext(ctx context.Context) (Caller, bool) {
	if ctx == nil {
		return Caller{}, false
	}
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok && caller.ExecutionID != "" && caller.Workload.Valid() && caller.User.Valid()
}
