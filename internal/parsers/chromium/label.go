package chromium

import "strings"

// labelOf decides the browser, the profile and the package from the logical path alone (4F-R5, 4F-R32): the last
// app_chrome or app_webview segment decides; the package is the segment before it and the profile the directory
// holding History when that is directly under it. Anything else is unknown-chromium, never chrome.
func labelOf(logical string) (browser, profile, pkg string) {
	path := strings.TrimPrefix(logical, "android:")
	parts := strings.Split(path, "/")
	at := -1
	for i, p := range parts {
		if p == "app_chrome" || p == "app_webview" {
			at = i
		}
	}
	if at < 0 {
		return "unknown-chromium", "", ""
	}
	if at >= 1 {
		pkg = parts[at-1]
	}
	if parts[len(parts)-1] == "History" && at == len(parts)-3 {
		profile = parts[at+1]
	}
	if parts[at] == "app_webview" {
		return "webview", profile, pkg
	}
	return browserOfPackage(pkg), profile, pkg
}

// browserOfPackage is the package table of 4F-R5 (from memory of public package identifiers: a limitation).
func browserOfPackage(pkg string) string {
	switch pkg {
	case "com.android.chrome", "com.chrome.beta", "com.chrome.dev", "com.chrome.canary":
		return "chrome"
	case "org.chromium.chrome":
		return "chromium"
	case "com.brave.browser", "com.brave.browser_beta", "com.brave.browser_nightly":
		return "brave"
	case "com.microsoft.emmx", "com.microsoft.emmx.beta", "com.microsoft.emmx.dev", "com.microsoft.emmx.canary":
		return "edge"
	case "com.opera.browser", "com.opera.browser.beta":
		return "opera"
	}
	return "unknown-chromium"
}
