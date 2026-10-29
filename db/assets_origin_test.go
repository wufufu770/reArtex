package db

import (
	"testing"

	"github.com/Autumn-27/artex/internal/scopepolicy"
)

// TestAssetsOriginColumnExists pins the provenance column: after the schema is
// applied assets.origin exists, and the value domain is the one the shared
// scope layer expects ('platform' = platform-owned).
//
// Without the column the platform scope fails closed (the shared scope layer
// shows nothing at all), so the platform-owned mode is unusable.
func TestAssetsOriginColumnExists(t *testing.T) {
	d, _, _ := testSetup(t)
	defer d.Close()

	if !assetsOriginColumnExists(t, d) {
		t.Fatalf("assets.origin 列必须存在（schema.sql 落地后）")
	}

	var ddl string
	err := d.QueryRow(`SELECT data_type FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='assets' AND column_name='origin'`).Scan(&ddl)
	if err != nil {
		t.Fatal(err)
	}
	if ddl != "text" {
		t.Fatalf("assets.origin 类型 = %s，期望 text", ddl)
	}
}

// TestAssetsOriginDefaultsToAgentForExistingWrites pins that rows written by
// the existing insert path get a sensible provenance: 'agent' (probe-derived),
// which the platform scope does NOT pick up. Conservative by default — an
// existing write path must not accidentally become platform-owned.
func TestAssetsOriginDefaultsToAgentForExistingWrites(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	id, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "origin-default.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = assets.DeleteByIDs([]int64{id}) })

	var origin string
	if err := d.QueryRow(`SELECT origin FROM assets WHERE id=$1`, id).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin != "agent" {
		t.Fatalf("既有写入路径落库的 origin = %q，期望 'agent'（保守：不被平台范围命中）", origin)
	}
}

// TestPlatformScopeSeesOnlyPlatformOwnedAssets pins that the DDL makes the
// platform-owned mode usable: with origin='platform' the asset is visible in a
// context-less (platform) scope, while an 'agent'-origin asset is not.
func TestPlatformScopeSeesOnlyPlatformOwnedAssets(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	agentID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "origin-agent.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	platformID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "origin-platform.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = assets.DeleteByIDs([]int64{agentID, platformID}) })

	if _, err := d.Exec(`UPDATE assets SET origin='platform' WHERE id=$1`, platformID); err != nil {
		t.Fatal(err)
	}

	original := scopepolicy.Policy
	t.Cleanup(func() { scopepolicy.Policy = original })
	scopepolicy.Policy.PlatformScopeMode = scopepolicy.ScopePlatformOwned

	got, err := assets.ScopedAssetIDs(0, []int64{agentID, platformID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != platformID {
		t.Fatalf("平台范围应只看到 origin='platform' 的资产，实际 %v（agent=%d platform=%d）",
			got, agentID, platformID)
	}
}
