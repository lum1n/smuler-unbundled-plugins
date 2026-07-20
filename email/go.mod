module github.com/lum1n/smuler/plugins/email

go 1.26

require (
	github.com/emersion/go-imap v1.2.1
	github.com/lum1n/smuler/plugins/sdk-go v0.0.0
)

require (
	github.com/emersion/go-sasl v0.0.0-20200509203442-7bfe0ed36a21 // indirect
	github.com/lum1n/smuler/plugins/plugindebug v0.0.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace (
	github.com/lum1n/smuler/plugins/plugindebug => ../plugindebug
	github.com/lum1n/smuler/plugins/sdk-go => ../sdk-go
)
