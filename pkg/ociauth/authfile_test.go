package ociauth

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// We're using testscript, not for txtar tests,
	// but to access the test executable functionality.
	testscript.Main(m, map[string]func(){
		"docker-credential-test": helperMain,
	})
}

// patch temporarily replaces the value at ptr with val, restoring it via t.Cleanup.
func patch[T any](t *testing.T, ptr *T, val T) {
	old := *ptr
	*ptr = val
	t.Cleanup(func() { *ptr = old })
}

func TestLoadWithNoConfig(t *testing.T) {
	patch(t, &userHomeDir, func(getenv func(string) string) string {
		return getenv("HOME")
	})
	t.Setenv("HOME", "")
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	c, err := Load(noRunner)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("some.org")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{}, info)
}

func TestLoad(t *testing.T) {
	// Write config files in all the places, so we can check
	// that the precedence works OK.
	d := t.TempDir()
	patch(t, &userHomeDir, func(getenv func(string) string) string {
		return getenv("HOME")
	})
	locations := []struct {
		env      string
		dir      string
		file     string
		isInline bool
	}{
		{
			env:      "DOCKER_AUTH_CONFIG",
			isInline: true,
		},
		{
			env:  "DOCKER_CONFIG",
			dir:  "dockerconfig",
			file: "config.json",
		},
		{
			env:  "HOME",
			dir:  "home",
			file: ".docker/config.json",
		}, {
			env:  "XDG_RUNTIME_DIR",
			dir:  "xdg",
			file: "containers/auth.json",
		},
	}

	for _, loc := range locations {
		c := []byte(`
{
	"auths": {
		"someregistry.example.com": {
			"username": ` + fmt.Sprintf("%q", loc.env) + `,
			"password": "somepassword"
		}
	}
}`)
		if loc.isInline {
			// Inline config for DOCKER_AUTH_CONFIG.
			t.Setenv(loc.env, string(c))
		} else {
			epath := filepath.Join(d, loc.dir)
			t.Setenv(loc.env, epath)
			cfgPath := filepath.Join(epath, filepath.FromSlash(loc.file))
			err := os.MkdirAll(filepath.Dir(cfgPath), 0o777)
			require.NoError(t, err)

			// Write the config file with a username that
			// reflects where it's stored.
			err = os.WriteFile(cfgPath, c, 0o666)
			require.NoError(t, err)
		}
	}
	for _, loc := range locations {
		t.Run(loc.env, func(t *testing.T) {
			c, err := Load(noRunner)
			require.NoError(t, err)
			info, err := c.EntryForRegistry("someregistry.example.com")
			require.NoError(t, err)
			require.Equal(t, ConfigEntry{
				Username: loc.env,
				Password: "somepassword",
			}, info)

			if loc.isInline {
				// Remove the DOCKER_AUTH_CONFIG so that the next
				// level of precedence can be checked.
				err := os.Unsetenv(loc.env)
				require.NoError(t, err)
			} else {
				// Remove the directory containing the above
				// config file so that the next level of precedence
				// can be checked.
				err = os.RemoveAll(filepath.Join(d, loc.dir))
				require.NoError(t, err)
			}
		})
	}
	// When there's no config file available, it should return
	// an empty configuration and no error.
	c, err := Load(noRunner)
	require.NoError(t, err)

	info, err := c.EntryForRegistry("someregistry.example.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{}, info)
}

func TestWithBase64Auth(t *testing.T) {
	c, err := load(t, noRunner, `
{
	"auths": {
		"someregistry.example.com": {
			"auth": "dGVzdHVzZXI6cGFzc3dvcmQ="
		}
	}
}`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("someregistry.example.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "testuser",
		Password: "password",
	}, info)
}

func TestWithMalformedBase64Auth(t *testing.T) {
	_, err := load(t, noRunner, `
{
	"auths": {
		"someregistry.example.com": {
			"auth": "!!!"
		}
	}
}`)
	require.Error(t, err)
	require.Regexp(t, `invalid config file ".*": cannot decode auth field for "someregistry.example.com": invalid base64-encoded string`, err.Error())
}

func TestWithAuthAndUsername(t *testing.T) {
	// An auth field overrides the username/password pair.
	c, err := load(t, noRunner, `
{
	"auths": {
		"someregistry.example.com": {
			"auth": "dGVzdHVzZXI6cGFzc3dvcmQ=",
			"username": "foo",
			"password": "bar"
		}
	}
}`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("someregistry.example.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "testuser",
		Password: "password",
	}, info)
}

func TestWithURLEntry(t *testing.T) {
	c, err := load(t, noRunner, `
{
	"auths": {
		"https://someregistry.example.com/v1": {
			"username": "foo",
			"password": "bar"
		}
	}
}`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("someregistry.example.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "foo",
		Password: "bar",
	}, info)
}

func TestWithURLEntryAndExplicitHost(t *testing.T) {
	c, err := load(t, noRunner, `
{
	"auths": {
		"https://someregistry.example.com/v1": {
			"username": "foo",
			"password": "bar"
		},
		"someregistry.example.com": {
			"username": "baz",
			"password": "arble"
		}
	}
}`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("someregistry.example.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "baz",
		Password: "arble",
	}, info)
	info, err = c.EntryForRegistry("https://someregistry.example.com/v1")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "foo",
		Password: "bar",
	}, info)
}

func TestWithMultipleURLsAndSameHost(t *testing.T) {
	c, err := load(t, noRunner, `
{
	"auths": {
		"https://someregistry.example.com/v1": {
			"username": "u1",
			"password": "p"
		},
		"http://someregistry.example.com/v1": {
			"username": "u2",
			"password": "p"
		},
		"http://someregistry.example.com/v2": {
			"username": "u3",
			"password": "p"
		}
	}
}`)
	require.NoError(t, err)
	_, err = c.EntryForRegistry("someregistry.example.com")
	require.Error(t, err)
	require.Regexp(t, `more than one auths entry for "someregistry.example.com" \(http://someregistry.example.com/v1, http://someregistry.example.com/v2, https://someregistry.example.com/v1\)`, err.Error())
}

func TestWithHelperBasic(t *testing.T) {
	// Note: "test" matches the executable installed using testscript in RunMain.
	c, err := load(t, nil, `
{
	"credHelpers": {
		"registry-with-basic-auth.com": "test"
	}
}
`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("registry-with-basic-auth.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "someuser",
		Password: "somesecret",
	}, info)
}

func TestWithHelperToken(t *testing.T) {
	// Note: "test" matches the executable installed using testscript in RunMain.
	c, err := load(t, nil, `
{
	"credHelpers": {
		"registry-with-token.com": "test"
	}
}
`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("registry-with-token.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		RefreshToken: "sometoken",
	}, info)
}

func TestWithHelperRegistryNotFound(t *testing.T) {
	// Note: "test" matches the executable installed using testscript in RunMain.
	c, err := load(t, nil, `
{
	"credHelpers": {
		"other.com": "test"
	}
}
`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("other.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{}, info)
}

func TestWithHelperRegistryOtherError(t *testing.T) {
	// Note: "test" matches the executable installed using testscript in RunMain.
	c, err := load(t, nil, `
{
	"credHelpers": {
		"registry-with-error.com": "test"
	}
}
`)
	require.NoError(t, err)
	_, err = c.EntryForRegistry("registry-with-error.com")
	require.Error(t, err)
	require.Regexp(t, `error getting credentials: some error`, err.Error())
}

func TestWithDefaultHelper(t *testing.T) {
	// Note: "test" matches the executable installed using testscript in RunMain.
	c, err := load(t, nil, `
{
	"credsStore": "test"
}
`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("registry-with-basic-auth.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "someuser",
		Password: "somesecret",
	}, info)
}

func TestWithDefaultHelperNotFound(t *testing.T) {
	// When there's a helper not associated with any specific
	// host, it ignores the fact that the executable isn't
	// found and uses the regular "auths" info.
	// See https://github.com/cue-lang/cue/issues/2934.
	c, err := load(t, nil, `
{
	"credsStore": "definitely-not-found-executable",
	"auths": {
		"registry-with-basic-auth.com": {
			"username": "u1",
			"password": "p"
		}
	}
}
`)
	require.NoError(t, err)
	info, err := c.EntryForRegistry("registry-with-basic-auth.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "u1",
		Password: "p",
	}, info)
}

func TestWithDefaultHelperOtherError(t *testing.T) {
	// When there's a helper not associated with any specific
	// host, it's still an error if it's any error other than HelperNotFound.
	errHelper := func(helperName string, serverURL string) (ConfigEntry, error) {
		return ConfigEntry{}, fmt.Errorf("some error")
	}
	c, err := load(t, errHelper, `
{
	"credsStore": "test",
	"auths": {
		"registry-with-basic-auth.com": {
			"username": "u1",
			"password": "p"
		}
	}
}
`)
	require.NoError(t, err)
	_, err = c.EntryForRegistry("registry-with-basic-auth.com")
	require.Error(t, err)
	require.Regexp(t, `some error`, err.Error())
}

func TestWithSpecificHelperNotFound(t *testing.T) {
	// When there's a helper specifically configured for a host,
	// it _is_ an error that the helper isn't found.
	c, err := load(t, nil, `
{
	"credHelpers": {
		"registry-with-basic-auth.com": "definitely-not-found-executable"
	}
}
`)
	require.NoError(t, err)
	_, err = c.EntryForRegistry("registry-with-basic-auth.com")
	require.Error(t, err)
	require.Regexp(t, `helper not found: exec: "docker-credential-definitely-not-found-executable": executable file not found .*`, err.Error())
}

func TestWithHelperAndExplicitEnv(t *testing.T) {
	d := t.TempDir()
	// Note: "test" matches the executable installed using testscript in RunMain.
	err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`
{
	"credHelpers": {
		"registry-with-env-lookup.com": "test"
	}
}
`), 0o666)
	require.NoError(t, err)
	c, err := LoadWithEnv(nil, []string{
		"DOCKER_CONFIG=" + d,
		"TEST_SECRET=foo",
	})
	require.NoError(t, err)
	info, err := c.EntryForRegistry("registry-with-env-lookup.com")
	require.NoError(t, err)
	require.Equal(t, ConfigEntry{
		Username: "someuser",
		Password: "foo",
	}, info)
}

func load(t *testing.T, runner HelperRunner, cfgData string) (Config, error) {
	d := t.TempDir()
	t.Setenv("DOCKER_CONFIG", d)
	err := os.WriteFile(filepath.Join(d, "config.json"), []byte(cfgData), 0o666)
	require.NoError(t, err)
	return Load(runner)
}

func noRunner(helperName string, serverURL string) (ConfigEntry, error) {
	panic("no helpers available")
}

// helperMain implements a docker credential command main function.
func helperMain() {
	flag.Parse()
	if flag.NArg() != 1 || flag.Arg(0) != "get" {
		log.Fatal("usage: docker-credential-test get")
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}
	switch string(input) {
	case "registry-with-basic-auth.com":
		fmt.Printf(`
{
	"Username": "someuser",
	"Secret": "somesecret"
}`)
	case "registry-with-token.com":
		fmt.Printf(`
{
	"Username": "<token>",
	"Secret": "sometoken"
}
`)
	case "registry-with-env-lookup.com":
		fmt.Printf(`
{
	"Username": "someuser",
	"Secret": %q
}`, os.Getenv("TEST_SECRET"))
	case "registry-with-error.com":
		fmt.Fprintf(os.Stderr, "some error\n")
		os.Exit(1)
	default:
		fmt.Printf("credentials not found in native keychain\n")
		os.Exit(1)
	}
}
