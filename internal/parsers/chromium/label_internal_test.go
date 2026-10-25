package chromium

import "testing"

// The browser, profile and package come from the logical path alone, never from the data (4F-R5, 4F-R32).
func TestBrowserAndProfileFromThePath(t *testing.T) {
	rows := []struct{ logical, browser, profile, pkg string }{
		{"android:/data/data/com.android.chrome/app_chrome/Default/History", "chrome", "Default", "com.android.chrome"},
		{"android:/data/data/com.chrome.beta/app_chrome/Default/History", "chrome", "Default", "com.chrome.beta"},
		{"android:/data/data/com.chrome.dev/app_chrome/Default/History", "chrome", "Default", "com.chrome.dev"},
		{"android:/data/data/com.chrome.canary/app_chrome/Default/History", "chrome", "Default", "com.chrome.canary"},
		{"android:/data/data/org.chromium.chrome/app_chrome/Default/History", "chromium", "Default", "org.chromium.chrome"},
		{"android:/data/user/10/com.brave.browser/app_chrome/Profile 1/History", "brave", "Profile 1", "com.brave.browser"},
		{"android:/data/data/com.brave.browser_beta/app_chrome/Default/History", "brave", "Default", "com.brave.browser_beta"},
		{"android:/data/data/com.brave.browser_nightly/app_chrome/Default/History", "brave", "Default", "com.brave.browser_nightly"},
		{"android:/data/data/com.microsoft.emmx/app_chrome/Default/History", "edge", "Default", "com.microsoft.emmx"},
		{"android:/data/data/com.microsoft.emmx.beta/app_chrome/Default/History", "edge", "Default", "com.microsoft.emmx.beta"},
		{"android:/data/data/com.microsoft.emmx.dev/app_chrome/Default/History", "edge", "Default", "com.microsoft.emmx.dev"},
		{"android:/data/data/com.microsoft.emmx.canary/app_chrome/Default/History", "edge", "Default", "com.microsoft.emmx.canary"},
		{"android:/data/data/com.opera.browser/app_chrome/Default/History", "opera", "Default", "com.opera.browser"},
		{"android:/data/data/com.opera.browser.beta/app_chrome/Default/History", "opera", "Default", "com.opera.browser.beta"},
		// the segment after the package decides: a webview directory is a webview whatever the package
		{"android:/data/data/com.android.chrome/app_webview/Default/History", "webview", "Default", "com.android.chrome"},
		{"android:/data/data/com.example.reader/app_webview/Default/History", "webview", "Default", "com.example.reader"},
		{"android:/data/data/com.example.reader/app_webview/Default/Sub/History", "webview", "", "com.example.reader"},
		{"android:/data/data/com.example.reader/app_webview/History", "webview", "", "com.example.reader"},
		// an unrecognised package is never labelled chrome
		{"android:/data/data/com.example.x/app_chrome/Default/History", "unknown-chromium", "Default", "com.example.x"},
		{"android:/data/data/com.android.chrome.fake/app_chrome/Default/History", "unknown-chromium", "Default", "com.android.chrome.fake"},
		{"android:/data/data/com.android.chrome/app_chrome/History", "chrome", "", "com.android.chrome"},
		{"android:/data/data/com.android.chrome/app_chrome/Default/Other", "chrome", "", "com.android.chrome"},
		{"android:/sdcard/History", "unknown-chromium", "", ""},
		{"", "unknown-chromium", "", ""},
		// several app segments: the last one decides (4F-R32)
		{"android:/data/data/com.android.chrome/app_webview/x/com.brave.browser/app_chrome/Default/History", "brave", "Default", "com.brave.browser"},
		{"android:/data/data/com.brave.browser/app_chrome/x/com.android.chrome/app_webview/Default/History", "webview", "Default", "com.android.chrome"},
	}
	for _, r := range rows {
		b, p, k := labelOf(r.logical)
		if b != r.browser || p != r.profile || k != r.pkg {
			t.Errorf("labelOf(%q) = %q, %q, %q; want %q, %q, %q", r.logical, b, p, k, r.browser, r.profile, r.pkg)
		}
	}
}
