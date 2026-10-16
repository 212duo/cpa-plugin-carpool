module github.com/212duo/cpa-plugin-carpool

go 1.24.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.34.4
)

// Build against the CLIProxyAPI source checkout matching the deployed image.
replace github.com/router-for-me/CLIProxyAPI/v8 => ../CLIProxyAPI
