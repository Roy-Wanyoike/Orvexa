package registry

// fieldRef is a writable view of one configuration field, used by the env
// parser and the validators. Credential fields store Secret values while
// endpoint fields (FreeSWITCH/Asterisk host and port) store plain strings;
// fieldRef lets one parse/validate loop drive both without downgrading any
// credential to plain string storage — the Secret typing that every
// redaction path depends on is preserved.
//
// A fieldRef is only valid while its owning config struct lives; the configs
// are heap-allocated value holders, never moved, so the internal pointers are
// stable. Coverage tests pin that every FieldSpec env var maps to exactly one
// distinct field.
type fieldRef struct {
	secret *Secret
	plain  *string
}

func secretRef(s *Secret) fieldRef { return fieldRef{secret: s} }
func plainRef(s *string) fieldRef  { return fieldRef{plain: s} }

// get reads the field's current value.
func (f fieldRef) get() string {
	if f.secret != nil {
		return string(*f.secret)
	}
	return *f.plain
}

// set writes a parsed value into the field.
func (f fieldRef) set(v string) {
	if f.secret != nil {
		*f.secret = Secret(v)
		return
	}
	*f.plain = v
}
