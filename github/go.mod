module github.com/lum1n/smuler/plugins/github

go 1.21

require (
	github.com/lum1n/smuler/plugins/httphealth v0.0.0
	github.com/lum1n/smuler/plugins/plugindebug v0.0.0
)

replace github.com/lum1n/smuler/plugins/httphealth => ../httphealth

replace github.com/lum1n/smuler/plugins/plugindebug => ../plugindebug
