package database

import (
	"strings"
	"testing"
)

func TestProvisionSQL(t *testing.T) {
	sql, err := ProvisionSQL("wp_ab12_db", "wp_ab12_usr", "Str0ng'pass\\word")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sql, "\n")
	if !strings.Contains(joined, "CREATE DATABASE IF NOT EXISTS `wp_ab12_db`") {
		t.Error("missing create database")
	}
	if !strings.Contains(joined, "ON `wp_ab12_db`.* TO 'wp_ab12_usr'@'%'") {
		t.Error("grant must be scoped to the site's own database only")
	}
	if strings.Contains(joined, "ON *.*") {
		t.Error("grant must never be global")
	}
	if strings.Contains(joined, "Str0ng'pass") {
		t.Error("password literal was not escaped")
	}
}

func TestProvisionSQLRejectsInjection(t *testing.T) {
	if _, err := ProvisionSQL("db;DROP DATABASE mysql", "user1", "x"); err == nil {
		t.Error("expected injected db name rejected")
	}
	if _, err := ProvisionSQL("valid_db", "user';--", "x"); err == nil {
		t.Error("expected injected user rejected")
	}
	if _, err := DropDatabaseSQL("valid_db", "bad user"); err == nil {
		t.Error("expected bad user rejected")
	}
	if _, err := SizeQuery("bad`db"); err == nil {
		t.Error("expected bad db rejected in size query")
	}
}
