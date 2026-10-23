package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
)

type authenticationJobs struct {
	calls   int
	options jobs.Options
	result  any
}

func (j *authenticationJobs) ResolveProgram(_ context.Context, id string) (packages.ProgramDefinition, error) {
	return packages.ProgramDefinition{ID: id, PackageID: "the8020/users", Commit: "active", EntrypointURL: authenticationModule}, nil
}

func (j *authenticationJobs) Run(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
	j.calls++
	j.options = options
	return jobs.Record{Result: j.result}, nil
}

func TestNativeAuthenticationUsesOrdinarySystemProgram(t *testing.T) {
	jobRunner := &authenticationJobs{result: true}
	runner, err := programs.New(jobRunner, jobRunner)
	if err != nil {
		t.Fatal(err)
	}
	signing, err := auth.OpenSigner(filepath.Join(t.TempDir(), "keys", "signing.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &packageAuthentication{context: context.Background(), programs: runner, signing: signing}
	// A password identical to the username must not corrupt the policy result
	// when the job machinery redacts secure inputs from returned strings.
	user, err := adapter.AuthenticatePassword("alice", []byte("alice"))
	if err != nil || user.Username != "alice" {
		t.Fatalf("password identity=%v err=%v", user, err)
	}
	options := jobRunner.options
	if options.User != execution.SystemUser() || options.Timeout != 30*time.Second || options.PlacementGroup != nil || len(options.Mounts) != 0 || options.Reuse != nil {
		t.Fatal("authentication did not use ordinary system-job policy")
	}
	if !reflect.DeepEqual(options.Arguments, []any{"password", "alice"}) || options.Secrets["password"] != "alice" {
		t.Fatal("password must be a secure input, separate from positional arguments")
	}
	for _, result := range []any{false, "alice", nil} {
		jobRunner.result = result
		if _, err := adapter.AuthenticateUser("alice"); err == nil {
			t.Fatal("only an affirmative package decision may authenticate")
		}
	}
	before := jobRunner.calls
	if _, err := adapter.AuthenticateToken(context.Background(), "invalid"); err == nil || jobRunner.calls != before {
		t.Fatal("invalid console tokens must be rejected before job execution")
	}
	jobRunner.result = true
	now := time.Now().Unix()
	claims := auth.TokenClaims{"iss": auth.TokenIssuer, "aud": auth.TokenAudience, "sub": "user:alice", "sid": "session", "ver": 1, "iat": now, "exp": now + 60}
	token, err := signing.SignToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	user, err = adapter.AuthenticateToken(context.Background(), token)
	if err != nil || user.Username != "alice" {
		t.Fatalf("session identity=%v err=%v", user, err)
	}
	var forwarded auth.TokenClaims
	if err := json.Unmarshal([]byte(jobRunner.options.Secrets["claims"]), &forwarded); err != nil || forwarded["sub"] != user.ID || !reflect.DeepEqual(jobRunner.options.Arguments, []any{"session", "alice"}) {
		t.Fatal("console must send verified claims through ordinary secure inputs")
	}
}
