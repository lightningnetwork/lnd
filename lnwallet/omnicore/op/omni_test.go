package op

import (
	"bytes"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestEnode(t *testing.T) {
	op := &Opreturn{Outputs: []*Output{
		&Output{Index: 2, Amount: 10.5 * 100000000},
		&Output{Index: 3, Amount: 0.5 * 100000000},
		&Output{Index: 5, Amount: 15 * 100000000}},
		PropertyId: 31,
	}
	w := bytes.NewBuffer([]byte{})
	err := op.Encode(w)
	require.Nil(t, err)
	t.Logf("%x", w.Bytes())

	//decode opreturn_payload  and json print
	PrintWithDecode(w.Bytes())

	require.Equal(t, "000000070000001f0302000000003e95ba80030000000002faf080050000000059682f00", fmt.Sprintf("%x", w.Bytes()))

	//opr := new(Opreturn)
	//err = opr.Decode(w.Bytes())
	//require.Nil(t, err)
	//jsonOut, _ := json.MarshalIndent(opr, "", "  ")
	//t.Log(string(jsonOut))

}
