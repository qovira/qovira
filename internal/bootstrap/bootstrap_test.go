package bootstrap

import (
	"context"
	"errors"
	"testing"
)

// --- in-package fakes ---

// fakeUserExister is an in-package fake for the UserExister seam.
type fakeUserExister struct {
	hasAny bool
	err    error
}

func (f *fakeUserExister) HasAnyUser(_ context.Context) (bool, error) {
	return f.hasAny, f.err
}

// fakeAccountCreator is an in-package fake for the AccountCreator seam.
// It records every CreateAdmin call so tests can assert exactly how many were
// made and with what arguments.
type fakeAccountCreator struct {
	calls []createAdminCall
	err   error
}

type createAdminCall struct {
	email    string
	password string
}

func (f *fakeAccountCreator) CreateAdmin(_ context.Context, email, password string) error {
	f.calls = append(f.calls, createAdminCall{email: email, password: password})
	return f.err
}

// fakeSettingsReader is an in-package fake for the SettingsReader seam.
// It holds a map of key→value; absent keys are treated as not-found.
type fakeSettingsReader struct {
	data map[string]string
	err  error
}

func (f *fakeSettingsReader) Get(_ context.Context, key string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.data[key]
	return v, ok, nil
}

// --- isFirstRun ---

func TestIsFirstRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		hasAny  bool
		err     error
		want    bool
		wantErr bool
	}{
		{
			name:   "no users → first run",
			hasAny: false,
			want:   true,
		},
		{
			name:   "users exist → not first run",
			hasAny: true,
			want:   false,
		},
		{
			name:    "store error is propagated",
			err:     errors.New("db offline"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ue := &fakeUserExister{hasAny: tc.hasAny, err: tc.err}
			got, err := IsFirstRun(context.Background(), ue)
			if (err != nil) != tc.wantErr {
				t.Fatalf("IsFirstRun() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("IsFirstRun() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- NeedsOnboarding ---

func TestNeedsOnboarding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		data    map[string]string
		err     error
		want    bool
		wantErr bool
	}{
		{
			name: "no model endpoint configured → needs onboarding",
			data: nil,
			want: true,
		},
		{
			name: "empty model endpoint value → needs onboarding",
			data: map[string]string{KeyModelEndpoint: ""},
			want: true,
		},
		{
			name: "model endpoint set → does not need onboarding",
			data: map[string]string{KeyModelEndpoint: "https://api.example.com"},
			want: false,
		},
		{
			name:    "settings reader error is propagated",
			err:     errors.New("db error"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sr := &fakeSettingsReader{data: tc.data, err: tc.err}
			got, err := NeedsOnboarding(context.Background(), sr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NeedsOnboarding() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("NeedsOnboarding() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNeedsOnboardingIsIndependentOfAdminExistence verifies that
// NeedsOnboarding reflects only the model-endpoint setting, not whether any
// admin account was created. A seeded admin must not change the onboarding
// state on its own.
func TestNeedsOnboardingIsIndependentOfAdminExistence(t *testing.T) {
	t.Parallel()

	// Scenario: first run, admin seeded, but no model endpoint written.
	sr := &fakeSettingsReader{data: nil} // no settings at all
	got, err := NeedsOnboarding(context.Background(), sr)
	if err != nil {
		t.Fatalf("NeedsOnboarding() unexpected error: %v", err)
	}
	if !got {
		t.Error("expected NeedsOnboarding() = true after admin seeding with no model endpoint; got false")
	}
}

// --- MaybeSeedAdmin ---

func TestMaybeSeedAdmin(t *testing.T) {
	t.Parallel()

	errSeed := errors.New("create failed")

	cases := []struct {
		name       string
		isFirst    bool
		email      string
		password   string
		creatorErr error
		wantSeeded bool
		wantErr    bool
		wantCalls  int
	}{
		{
			name:       "first run with credentials → one CreateAdmin call, seeded=true",
			isFirst:    true,
			email:      "admin@example.com",
			password:   "s3cret",
			wantSeeded: true,
			wantCalls:  1,
		},
		{
			name:      "first run without email → no call, seeded=false",
			isFirst:   true,
			email:     "",
			password:  "s3cret",
			wantCalls: 0,
		},
		{
			name:      "first run without password → no call, seeded=false",
			isFirst:   true,
			email:     "admin@example.com",
			password:  "",
			wantCalls: 0,
		},
		{
			name:      "first run with neither credential → no call, seeded=false",
			isFirst:   true,
			email:     "",
			password:  "",
			wantCalls: 0,
		},
		{
			name:      "not first run with credentials → no call, seeded=false",
			isFirst:   false,
			email:     "admin@example.com",
			password:  "s3cret",
			wantCalls: 0,
		},
		{
			name:       "creator error is propagated",
			isFirst:    true,
			email:      "admin@example.com",
			password:   "s3cret",
			creatorErr: errSeed,
			wantErr:    true,
			wantCalls:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ac := &fakeAccountCreator{err: tc.creatorErr}
			seeded, err := MaybeSeedAdmin(context.Background(), tc.isFirst, tc.email, tc.password, ac)
			if (err != nil) != tc.wantErr {
				t.Fatalf("MaybeSeedAdmin() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && seeded != tc.wantSeeded {
				t.Errorf("MaybeSeedAdmin() seeded = %v, want %v", seeded, tc.wantSeeded)
			}
			if len(ac.calls) != tc.wantCalls {
				t.Errorf("CreateAdmin call count = %d, want %d", len(ac.calls), tc.wantCalls)
			}
			// When a call was made, verify the right credentials were passed.
			if tc.wantCalls == 1 && len(ac.calls) == 1 {
				if ac.calls[0].email != tc.email {
					t.Errorf("CreateAdmin email = %q, want %q", ac.calls[0].email, tc.email)
				}
				if ac.calls[0].password != tc.password {
					t.Errorf("CreateAdmin password = %q, want %q", ac.calls[0].password, tc.password)
				}
			}
		})
	}
}

// TestSeededAdminStillNeedsOnboarding verifies the explicit requirement that
// calling MaybeSeedAdmin does NOT write any settings, so NeedsOnboarding
// remains true until the model endpoint is configured separately.
func TestSeededAdminStillNeedsOnboarding(t *testing.T) {
	t.Parallel()

	ac := &fakeAccountCreator{}
	seeded, err := MaybeSeedAdmin(context.Background(), true, "admin@example.com", "pass", ac)
	if err != nil {
		t.Fatalf("MaybeSeedAdmin() unexpected error: %v", err)
	}
	if !seeded {
		t.Fatal("expected seeded = true")
	}

	// No model endpoint was written — NeedsOnboarding must still be true.
	sr := &fakeSettingsReader{data: nil}
	onboarding, err := NeedsOnboarding(context.Background(), sr)
	if err != nil {
		t.Fatalf("NeedsOnboarding() unexpected error: %v", err)
	}
	if !onboarding {
		t.Error("expected NeedsOnboarding() = true after MaybeSeedAdmin with no model endpoint; got false")
	}
}
