package jobkit

// PayloadObject decodes a job payload that a handler reads with
// `payload.get(...)`. A payload that is not a JSON object fails with the
// AttributeError text Python raises for it, so the recorded job error is the
// same whichever executor ran the attempt.
func PayloadObject(raw []byte) (*Object, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, Errorf("payload is not valid JSON: %v", err)
	}
	obj, ok := v.(*Object)
	if !ok {
		return nil, Errorf("'%s' object has no attribute 'get'", TypeName(v))
	}
	return obj, nil
}

// Field is `payload.get(key)` rendered with Str: the string a handler would
// compare against, or "" when the key is absent or falsy.
func (o *Object) Field(key string) string {
	v, _ := o.Get(key)
	return Str(v)
}
