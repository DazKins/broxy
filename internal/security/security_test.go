package security

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("secret-value")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if err := CheckPassword(hash, "secret-value"); err != nil {
		t.Fatalf("CheckPassword() error = %v", err)
	}
}

func TestNormalizeBearer(t *testing.T) {
	token := NormalizeBearer("Bearer abc123")
	if token != "abc123" {
		t.Fatalf("NormalizeBearer() = %q, want %q", token, "abc123")
	}
}
