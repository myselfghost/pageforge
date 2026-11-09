package cache

import "strings"

// Object identifies one project file in S3. The tuple is the cache key:
// the same physical object is shared by URL bindings, share links and
// version direct links, so none of those are part of the identifier.
type Object struct {
	AliasID   string
	Bucket    string
	ProjectID string
	VersionID string
	// Key is the path inside the version, for example "assets/app.js".
	Key string
}

// ID returns the in-memory key used by the index and the negative cache.
func (o Object) ID() string {
	return o.AliasID + "\x00" + o.Bucket + "\x00" + o.ProjectID + "\x00" + o.VersionID + "\x00" + o.Key
}

// segments renders the object as validated path segments that mirror the
// object-storage layout: alias_id/bucket/project_id/version_id/object_key.
func (o Object) segments() ([]string, bool) {
	prefix := []string{o.AliasID, o.Bucket, o.ProjectID, o.VersionID}
	for _, segment := range prefix {
		if !safeSegment(segment) {
			return nil, false
		}
	}
	key := strings.TrimPrefix(o.Key, "/")
	if key == "" {
		return nil, false
	}
	parts := strings.Split(key, "/")
	out := make([]string, 0, len(prefix)+len(parts))
	out = append(out, prefix...)
	for _, part := range parts {
		if !safeSegment(part) {
			return nil, false
		}
		out = append(out, part)
	}
	return out, true
}

// safeSegment reports whether one path segment can be used verbatim as a
// file-system name on Linux and Windows. Objects that fail this check are
// still served, they are just not cached.
func safeSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." || len(segment) > 255 {
		return false
	}
	last := segment[len(segment)-1]
	if last == '.' || last == ' ' {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case '\\', ':', '*', '?', '"', '<', '>', '|':
			return false
		}
	}
	return !reservedWindowsName(segment)
}

// reservedWindowsName reports whether segment is a device name such as CON or
// COM1 that Windows interprets specially even when it carries an extension.
func reservedWindowsName(segment string) bool {
	if len(segment) < 3 {
		return false
	}
	base := segment
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	upper := strings.ToUpper(base)
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(upper) != 4 {
		return false
	}
	if (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
		return true
	}
	return false
}
