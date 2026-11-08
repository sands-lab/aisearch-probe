package probe

// IDPageCodeID is the identity the personal page (/idpage/) makes its codes from:
// the page id plus ONE named query item and its value. Pass it to DepthCode in
// place of a plain page id. The origin and the runner both call this, so the codes
// a model quotes can be traced back to exactly whose copy it held.
//
// No value is the anonymous page. Its identity has no "=", so no person's value —
// not even one spelled "anonymous" — can produce it.
func IDPageCodeID(id, name, value string) string {
	if value == "" {
		return id + "|" + name + "|anonymous"
	}
	return id + "|" + name + "=" + value
}
