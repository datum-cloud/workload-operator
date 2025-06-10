package instancecontrol

type SchedulingGate string

const (
	NetworkSchedulingGate SchedulingGate = "Network"
)

func (s SchedulingGate) String() string {
	return string(s)
}
