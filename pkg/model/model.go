package model

type ProgressCallback func(interface{})

type Header struct {
	Desc string
	ID   uint8
}

type HeaderResult struct {
	Header
	Value string
}

func (t *HeaderResult) String() string {
	return t.Desc + ": " + t.Value
}