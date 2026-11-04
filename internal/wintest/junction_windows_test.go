package wintest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateJunctionWithCommandCharacters(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent &()^%![brackets]")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "target &()^%![brackets]")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link &()^%![brackets]")
	if err := CreateJunction(link, target); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(link, "keep")); err != nil || string(data) != "keep" {
		t.Fatalf("junction target=%q, %v", data, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(data) != "keep" {
		t.Fatalf("removed junction changed target=%q, %v", data, err)
	}
}
