package zooid

import (
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
)

func TestConfig_IsOwner(t *testing.T) {
	ownerPubkey := nostr.MustPubKeyFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	otherPubkey := nostr.MustPubKeyFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	config := &Config{}
	config.Info.Pubkey = ownerPubkey.Hex()

	if !config.IsOwner(ownerPubkey) {
		t.Error("IsOwner() should return true for owner pubkey")
	}

	if config.IsOwner(otherPubkey) {
		t.Error("IsOwner() should return false for non-owner pubkey")
	}
}

func TestConfig_IsSelf(t *testing.T) {
	secret := nostr.Generate()
	selfPubkey := secret.Public()
	otherPubkey := nostr.MustPubKeyFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	config := &Config{
		secret: secret,
	}

	if !config.IsSelf(selfPubkey) {
		t.Error("IsSelf() should return true for self pubkey")
	}

	if config.IsSelf(otherPubkey) {
		t.Error("IsSelf() should return false for non-self pubkey")
	}
}

func TestConfig_GetAllRoles(t *testing.T) {
	pubkey1 := nostr.MustPubKeyFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	pubkey2 := nostr.MustPubKeyFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	config := &Config{
		Roles: map[string]Role{
			"member": {
				Pubkeys:   []string{},
				CanInvite: true,
			},
			"admin": {
				Pubkeys:   []string{pubkey1.Hex()},
				CanManage: true,
			},
			"moderator": {
				Pubkeys:   []string{pubkey2.Hex()},
				CanInvite: true,
			},
		},
	}

	roles := config.GetAllRoles(pubkey1)
	if len(roles) != 2 {
		t.Errorf("GetAllRoles() returned %d roles, want 2", len(roles))
	}

	roles = config.GetAllRoles(pubkey2)
	if len(roles) != 2 {
		t.Errorf("GetAllRoles() returned %d roles, want 2", len(roles))
	}
}

func TestConfig_CanManage(t *testing.T) {
	ownerPubkey := nostr.MustPubKeyFromHex("9999999999999999999999999999999999999999999999999999999999999999")
	adminPubkey := nostr.MustPubKeyFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	userPubkey := nostr.MustPubKeyFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	config := &Config{
		secret: nostr.Generate(),
		Roles: map[string]Role{
			"admin": {
				Pubkeys:   []string{adminPubkey.Hex()},
				CanManage: true,
			},
			"user": {
				Pubkeys:   []string{userPubkey.Hex()},
				CanManage: false,
			},
		},
	}
	config.Info.Pubkey = ownerPubkey.Hex()

	if !config.CanManage(adminPubkey) {
		t.Error("CanManage() should return true for admin")
	}

	if config.CanManage(userPubkey) {
		t.Error("CanManage() should return false for regular user")
	}
}

func TestConfig_CanInvite(t *testing.T) {
	ownerPubkey := nostr.MustPubKeyFromHex("9999999999999999999999999999999999999999999999999999999999999999")
	inviterPubkey := nostr.MustPubKeyFromHex("1234567890123456789012345678901234567890123456789012345678901234")
	userPubkey := nostr.MustPubKeyFromHex("abcdef1234567890123456789012345678901234567890123456789012345678")

	config := &Config{
		secret: nostr.Generate(),
		Roles: map[string]Role{
			"inviter": {
				Pubkeys:   []string{inviterPubkey.Hex()},
				CanInvite: true,
			},
			"user": {
				Pubkeys:   []string{userPubkey.Hex()},
				CanInvite: false,
			},
		},
	}
	config.Info.Pubkey = ownerPubkey.Hex()

	if !config.CanInvite(inviterPubkey) {
		t.Error("CanInvite() should return true for inviter")
	}

	if config.CanInvite(userPubkey) {
		t.Error("CanInvite() should return false for regular user")
	}
}

func TestConfig_MemberRole(t *testing.T) {
	ownerPubkey := nostr.MustPubKeyFromHex("9999999999999999999999999999999999999999999999999999999999999999")
	anyPubkey := nostr.MustPubKeyFromHex("1234567890123456789012345678901234567890123456789012345678901234")

	config := &Config{
		secret: nostr.Generate(),
		Roles: map[string]Role{
			"member": {
				Pubkeys:   []string{},
				CanInvite: true,
			},
		},
	}
	config.Info.Pubkey = ownerPubkey.Hex()

	roles := config.GetAllRoles(anyPubkey)
	if len(roles) != 1 {
		t.Errorf("GetAllRoles() should return member role for any pubkey, got %d roles", len(roles))
	}

	if !config.CanInvite(anyPubkey) {
		t.Error("Any pubkey should have member role permissions")
	}
}

func TestValidateBlossomFileStorage(t *testing.T) {
	t.Run("blossom disabled skips validation", func(t *testing.T) {
		c := &Config{}
		c.Blossom.Enabled = false
		c.Blossom.Backend = "s3"
		normalizeBlossomConfig(c)
		if err := validateBlossomFileStorage(c); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("local storage needs no s3 fields", func(t *testing.T) {
		c := &Config{}
		c.Blossom.Enabled = true
		c.Blossom.Backend = "local"
		normalizeBlossomConfig(c)
		if err := validateBlossomFileStorage(c); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("s3 requires bucket region keys and secret", func(t *testing.T) {
		c := &Config{}
		c.Blossom.Enabled = true
		c.Blossom.Backend = "s3"
		c.Blossom.S3.Region = "us-east-1"
		normalizeBlossomConfig(c)
		if err := validateBlossomFileStorage(c); err == nil {
			t.Fatal("expected error for missing bucket and credentials")
		}

		c.Blossom.S3.Bucket = "b"
		c.Blossom.S3.AccessKey = "k"
		c.Blossom.S3.SecretKey = "s"
		normalizeBlossomConfig(c)
		if err := validateBlossomFileStorage(c); err != nil {
			t.Fatalf("expected nil with all s3 fields set, got %v", err)
		}
	})

	t.Run("invalid backend value", func(t *testing.T) {
		c := &Config{}
		c.Blossom.Enabled = true
		c.Blossom.Backend = "nfs"
		normalizeBlossomConfig(c)
		if err := validateBlossomFileStorage(c); err == nil {
			t.Fatal("expected error for unknown backend")
		}
	})
}

func TestLoadConfigFromPath_BlossomS3(t *testing.T) {
	sk := nostr.Generate()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "relay.toml")
	tomlBody := `host = "r.example.com"
schema = "myrelay"
secret = "` + sk.Hex() + `"
inactive = false

[info]
name = "n"
pubkey = "` + sk.Public().Hex() + `"

[blossom]
enabled = true
backend = "s3"

[blossom.s3]
region = "auto"
bucket = "test-bucket"
access_key = "AKIA"
secret_key = "topsecret"
endpoint = "http://127.0.0.1:9000"
`
	if err := os.WriteFile(path, []byte(tomlBody), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatalf("LoadConfigFromPath: %v", err)
	}
	if cfg.Blossom.S3.SecretKey != "topsecret" {
		t.Errorf("expected s3 secret_key retained in struct, got %q", cfg.Blossom.S3.SecretKey)
	}
	if cfg.Blossom.Backend != "s3" {
		t.Errorf("backend: got %q", cfg.Blossom.Backend)
	}
}
