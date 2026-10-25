package chromium

// The names of the download columns, as listed in the Constants table of tools/fixtures/schemas/chromium/CROSSCHECK.md
// (4F-R27; TestDownloadEnumNamesFollowTheCrossCheck compares them). A stored integer the table does not list is
// "unknown": no name is ever guessed. They are switches, not package-level tables (the purity rule P5).
func stateName(v int64) string {
	switch v {
	case 0:
		return "in_progress"
	case 1:
		return "complete"
	case 2:
		return "cancelled"
	case 3:
		return "bug_140687"
	case 4:
		return "interrupted"
	}
	return "unknown"
}

func dangerName(v int64) string {
	switch v {
	case 0:
		return "not_dangerous"
	case 1:
		return "dangerous_file"
	case 2:
		return "dangerous_url"
	case 3:
		return "dangerous_content"
	case 4:
		return "maybe_dangerous_content"
	case 5:
		return "uncommon_content"
	case 6:
		return "user_validated"
	case 7:
		return "dangerous_host"
	case 8:
		return "potentially_unwanted"
	case 9:
		return "allowlisted_by_policy"
	case 10:
		return "async_scanning"
	case 11:
		return "blocked_password_protected"
	case 12:
		return "blocked_too_large"
	case 13:
		return "sensitive_content_warning"
	case 14:
		return "sensitive_content_block"
	case 15:
		return "deep_scanned_safe"
	case 16:
		return "deep_scanned_opened_dangerous"
	case 17:
		return "prompt_for_scanning"
	case 18:
		return "blocked_unsupported_filetype"
	case 19:
		return "dangerous_account_compromise"
	case 20:
		return "deep_scanned_failed"
	case 21:
		return "prompt_for_local_password_scanning"
	case 22:
		return "async_local_password_scanning"
	case 23:
		return "blocked_scan_failed"
	case 24:
		return "forced_save_to_gdrive"
	case 25:
		return "forced_save_to_onedrive"
	}
	return "unknown"
}

func interruptName(v int64) string {
	switch v {
	case 0:
		return "none"
	case 1:
		return "file_failed"
	case 2:
		return "file_access_denied"
	case 3:
		return "file_no_space"
	case 5:
		return "file_name_too_long"
	case 6:
		return "file_too_large"
	case 7:
		return "file_virus_infected"
	case 10:
		return "file_transient_error"
	case 11:
		return "file_blocked"
	case 12:
		return "file_security_check_failed"
	case 13:
		return "file_too_short"
	case 14:
		return "file_hash_mismatch"
	case 15:
		return "file_same_as_source"
	case 20:
		return "network_failed"
	case 21:
		return "network_timeout"
	case 22:
		return "network_disconnected"
	case 23:
		return "network_server_down"
	case 24:
		return "network_invalid_request"
	case 30:
		return "server_failed"
	case 31:
		return "server_no_range"
	case 33:
		return "server_bad_content"
	case 34:
		return "server_unauthorized"
	case 35:
		return "server_cert_problem"
	case 36:
		return "server_forbidden"
	case 37:
		return "server_unreachable"
	case 38:
		return "server_content_length_mismatch"
	case 39:
		return "server_cross_origin_redirect"
	case 40:
		return "user_canceled"
	case 41:
		return "user_shutdown"
	case 50:
		return "crash"
	case 51:
		return "local_download_blocked"
	}
	return "unknown"
}
