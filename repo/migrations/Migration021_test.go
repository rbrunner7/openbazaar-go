package migrations_test

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"testing"

	"github.com/OpenBazaar/openbazaar-go/repo/migrations"
	"github.com/OpenBazaar/openbazaar-go/schema"
)

const pre21MigrationConfig = `{"Wallets":{
}}`

func TestMigration021(t *testing.T) {
	var testRepo, err = schema.NewCustomSchemaManager(schema.SchemaContext{
		DataPath:        schema.GenerateTempPath(),
		TestModeEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = testRepo.BuildSchemaDirectories(); err != nil {
		t.Fatal(err)
	}
	defer testRepo.DestroySchemaDirectories()

	var (
		configPath  = testRepo.DataPathJoin("config")
		repoverPath = testRepo.DataPathJoin("repover")
	)
	if err = ioutil.WriteFile(configPath, []byte(pre21MigrationConfig), os.ModePerm); err != nil {
		t.Fatal(err)
	}

	if err = ioutil.WriteFile(repoverPath, []byte("21"), os.ModePerm); err != nil {
		t.Fatal(err)
	}

	var m migrations.Migration021
	err = m.Up(testRepo.DataPath(), "", true)
	if err != nil {
		t.Fatal(err)
	}

	configBytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var configInterface map[string]json.RawMessage
	if err = json.Unmarshal(configBytes, &configInterface); err != nil {
		t.Fatalf("unmarshaling invalid json after migration: %s", err.Error())
	}

	walletsMessage, ok := configInterface["Wallets"]
	if !ok {
		t.Error("missing expected 'Wallets' key after migration")
	}

	var walletsInterface map[string]json.RawMessage
	if err = json.Unmarshal(walletsMessage, &walletsInterface); err != nil {
		t.Fatalf("unmarshaling invalid json after migration: %s", err.Error())
	}

	_, ok = walletsInterface["XMR"]
	if !ok {
		t.Error("missing expected 'XMR' wallet key after migration")
	}

	assertCorrectRepoVer(t, repoverPath, "22")

	err = m.Down(testRepo.DataPath(), "", true)
	if err != nil {
		t.Fatal(err)
	}

	configBytes2, err := ioutil.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInterface2 := make(map[string]json.RawMessage)
	if err = json.Unmarshal(configBytes2, &configInterface2); err != nil {
		t.Fatalf("unmarshaling invalid json after migration: %s", err.Error())
	}

	walletsMessage2, ok := configInterface2["Wallets"]
	if !ok {
		t.Error("missing expected 'Wallets' key after migration")
	}

	var walletsInterface2 map[string]json.RawMessage
	if err = json.Unmarshal(walletsMessage2, &walletsInterface2); err != nil {
		t.Fatalf("unmarshaling invalid json after migration: %s", err.Error())
	}

	_, ok = walletsInterface2["XMR"]
	if ok {
		t.Error("expected 'XMR' wallet key to be removed after reversing migration")
	}

	assertCorrectRepoVer(t, repoverPath, "21")
}
