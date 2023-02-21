package proxy

import (
	"fmt"
	"testing"
)

func Test0001(t *testing.T) {
	src := "aaabsfasasdfasfas3rdsfasfdhsafdshsafafafafaffasfsafa1111111111111111111111111111111111111111111111111111111111"
	f := &ProxyFrame{}
	f.Type = FRAME_TYPE_DATA
	f.DataFrame = &DataFrame{}
	f.DataFrame.Data = []byte(src)
	fmt.Println(len(f.DataFrame.Data))
	b, err := MarshalSrpFrame(f, 10, "123123")
	if err != nil {
		t.Error(err)
	}
	fmt.Println(len(f.DataFrame.Data))
	ff, err := UnmarshalSrpFrame(b, "123123")
	if err != nil {
		t.Error(err)
	}
	if string(ff.DataFrame.Data) != src {
		t.Error(err)
	}
	fmt.Println(string(ff.DataFrame.Data))
}
