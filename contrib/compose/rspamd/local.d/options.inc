# Development only, mounted by compose.yaml. Besides GTUBE, which is always
# rejected, this makes Rspamd honour the test patterns for its other actions:
# the GTUBE string starting with YJS instead of XJS gets "add header", with
# ZJS "rewrite subject". The end-to-end test uses YJS to see spam land in
# Junk. Do not use this in production.
enable_test_patterns = true;
