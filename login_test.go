package main

import "testing"

func TestAppleFailure(t *testing.T) {
	ft, cm := appleFailure(`<key>failureType</key>
	<string>-5000</string>
	<key>customerMessage</key><string>Your Apple ID or password was entered incorrectly.</string>`)
	if ft != "-5000" || cm == "" {
		t.Fatal(ft, cm)
	}
	ft, cm = appleFailure(`{"failureType":"","customerMessage":"MZFinance.BadLogin.Configurator_message"}`)
	if ft != "" || cm != "MZFinance.BadLogin.Configurator_message" {
		t.Fatal(ft, cm)
	}
}
