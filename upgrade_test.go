package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGithubToken(t *testing.T) {
	// Snapshot and restore both env vars; the helper reads the live environment.
	keyGH, keyGITHUB := "GH_TOKEN", "GITHUB_TOKEN"
	prevGH, hadGH := os.LookupEnv(keyGH)
	prevGITHUB, hadGITHUB := os.LookupEnv(keyGITHUB)
	t.Cleanup(func() {
		os.Unsetenv(keyGH)
		os.Unsetenv(keyGITHUB)
		if hadGH {
			os.Setenv(keyGH, prevGH)
		}
		if hadGITHUB {
			os.Setenv(keyGITHUB, prevGITHUB)
		}
	})

	os.Unsetenv(keyGH)
	os.Unsetenv(keyGITHUB)
	assert.Equal(t, "", githubToken(), "no token set")

	os.Setenv(keyGITHUB, "ghp_github")
	os.Unsetenv(keyGH)
	assert.Equal(t, "ghp_github", githubToken(), "GITHUB_TOKEN fallback")

	os.Setenv(keyGH, "ghp_gh")
	assert.Equal(t, "ghp_gh", githubToken(), "GH_TOKEN takes precedence over GITHUB_TOKEN")
}

func TestAssetNameFor(t *testing.T) {
	cases := []struct {
		goos, goarch, tag string
		want              string
	}{
		{"darwin", "arm64", "v0.2.0", "kiro-anthropic_v0.2.0_darwin_arm64.tar.gz"},
		{"linux", "amd64", "v0.2.0", "kiro-anthropic_v0.2.0_linux_amd64.tar.gz"},
		{"windows", "amd64", "v0.2.0", "kiro-anthropic_v0.2.0_windows_amd64.zip"},
		{"windows", "arm64", "v1.0.0", "kiro-anthropic_v1.0.0_windows_arm64.zip"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, assetNameFor(c.goos, c.goarch, c.tag),
			"assetNameFor(%s,%s,%s)", c.goos, c.goarch, c.tag)
	}
}

func TestPickAsset(t *testing.T) {
	assets := []githubAsset{
		{Name: "kiro-anthropic_v0.2.0_darwin_amd64.tar.gz", URL: "u1"},
		{Name: "kiro-anthropic_v0.2.0_darwin_arm64.tar.gz", URL: "u2"},
		{Name: "kiro-anthropic_v0.2.0_linux_amd64.tar.gz", URL: "u3"},
		{Name: "kiro-anthropic_v0.2.0_windows_amd64.zip", URL: "u4"},
		{Name: "checksums.txt", URL: "uc"},
	}
	// exact match on the release tag
	got, err := pickAsset(assets, "darwin", "arm64", "v0.2.0")
	require.NoError(t, err)
	assert.Equal(t, "kiro-anthropic_v0.2.0_darwin_arm64.tar.gz", got.Name)

	got, err = pickAsset(assets, "windows", "amd64", "v0.2.0")
	require.NoError(t, err)
	assert.Equal(t, "kiro-anthropic_v0.2.0_windows_amd64.zip", got.Name)

	// unknown tag -> falls back to platform suffix match
	got, err = pickAsset(assets, "linux", "amd64", "")
	require.NoError(t, err)
	assert.Equal(t, "kiro-anthropic_v0.2.0_linux_amd64.tar.gz", got.Name)

	_, err = pickAsset(assets, "freebsd", "amd64", "v0.2.0")
	assert.Error(t, err, "pickAsset freebsd/amd64 should fail")
}

func TestParseChecksums(t *testing.T) {
	data := []byte("1111111111111111111111111111111111111111111111111111111111111111  kiro-anthropic_v0.2.0_darwin_arm64.tar.gz\n" +
		"2222222222222222222222222222222222222222222222222222222222222222  kiro-anthropic_v0.2.0_linux_amd64.tar.gz\n" +
		"3333333333333333333333333333333333333333333333333333333333333333 *kiro-anthropic_v0.2.0_windows_amd64.zip\n\n" +
		"garbage line with too many fields a b c\n")
	m := parseChecksums(data)
	require.Len(t, m, 3)
	assert.Equal(t, "1111111111111111111111111111111111111111111111111111111111111111",
		m["kiro-anthropic_v0.2.0_darwin_arm64.tar.gz"])
	// binary-mode "*name" prefix stripped
	assert.Equal(t, "3333333333333333333333333333333333333333333333333333333333333333",
		m["kiro-anthropic_v0.2.0_windows_amd64.zip"])
}

func TestVerifySHA256(t *testing.T) {
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	data := []byte("hello")
	sum := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	assert.NoError(t, verifySHA256(data, sum), "matching checksum")
	assert.Error(t, verifySHA256(data, "0000000000000000000000000000000000000000000000000000000000000000"),
		"mismatching checksum should fail")
	// case-insensitive
	assert.NoError(t, verifySHA256(data, "2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824"),
		"uppercase checksum should be accepted")
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.1", -1},
		{"v0.1.1", "v0.1.0", 1},
		{"v0.1.0", "v0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},      // missing v normalized
		{"v0.2.0", "v0.1.9", 1},     // semver precedence
		{"dev", "v0.1.0", -1},       // non-semver sorts below releases
		{"v0.1.0", "v0.1.0-rc1", 1}, // release > prerelease
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, compareVersions(c.a, c.b), "compareVersions(%q,%q)", c.a, c.b)
	}
}

func TestNewerReleases(t *testing.T) {
	all := []githubRelease{
		{TagName: "v0.5.1"},
		{TagName: "v0.4.0"},
		{TagName: "v0.5.0"},
		{TagName: "not-semver"}, // excluded
		{TagName: "v0.3.0"},
	}

	// current several versions behind: returns only newer, sorted newest-first.
	got := newerReleases(all, "0.4.0")
	require.Len(t, got, 2)
	assert.Equal(t, "v0.5.1", got[0].TagName)
	assert.Equal(t, "v0.5.0", got[1].TagName)

	// current equals latest: nothing newer.
	assert.Empty(t, newerReleases(all, "v0.5.1"))

	// current ahead of everything: nothing newer.
	assert.Empty(t, newerReleases(all, "v9.9.9"))

	// dev / non-semver current: never reports an update.
	assert.Empty(t, newerReleases(all, "dev"))

	// empty input.
	assert.Empty(t, newerReleases(nil, "v0.1.0"))
}

func TestCanonicalSemver(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":  "v0.1.0",
		"0.1.0":   "v0.1.0",
		"  v0.2 ": "v0.2",
		"":        "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, canonicalSemver(in), "canonicalSemver(%q)", in)
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"kiro-anthropic":            "kiro-anthropic",
		"./kiro-anthropic":          "kiro-anthropic",
		"dir/kiro-anthropic":        "kiro-anthropic",
		`C:\bin\kiro-anthropic.exe`: "kiro-anthropic.exe",
	}
	for in, want := range cases {
		assert.Equalf(t, want, baseName(in), "baseName(%q)", in)
	}
}

func TestTagFromLatestRedirect(t *testing.T) {
	cases := []struct {
		loc     string
		want    string
		wantErr bool
	}{
		{"https://github.com/YorrickBao/kiro-anthropic/releases/tag/v0.6.0", "v0.6.0", false},
		{"https://github.com/o/r/releases/tag/v1.2.3?foo=bar", "v1.2.3", false},
		{"/o/r/releases/tag/v0.1.0", "v0.1.0", false},
		// repo with no releases redirects to the releases index, not a tag.
		{"https://github.com/o/r/releases", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := tagFromLatestRedirect(c.loc)
		if c.wantErr {
			assert.Errorf(t, err, "tagFromLatestRedirect(%q)", c.loc)
			continue
		}
		require.NoErrorf(t, err, "tagFromLatestRedirect(%q)", c.loc)
		assert.Equalf(t, c.want, got, "tagFromLatestRedirect(%q)", c.loc)
	}
}

func TestTagIsNewer(t *testing.T) {
	assert.True(t, tagIsNewer("v0.5.0", "v0.6.0"))
	assert.True(t, tagIsNewer("0.5.0", "v0.6.0"))
	assert.False(t, tagIsNewer("v0.6.0", "v0.6.0"))
	assert.False(t, tagIsNewer("v0.6.0", "v0.5.0"))
	// dev / non-semver running build never reports an update.
	assert.False(t, tagIsNewer("dev", "v9.9.9"))
	assert.False(t, tagIsNewer("v0.5.0", "not-a-tag"))
}
