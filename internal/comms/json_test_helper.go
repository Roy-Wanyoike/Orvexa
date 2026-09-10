package comms

import "encoding/json"

func jsonUnmarshal(b []byte, dst any) error { return json.Unmarshal(b, dst) }
