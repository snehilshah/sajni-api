package main

func getExpenseReport(e expense) (string, float64) {
	switch v := e.(type) {
	case email:
		return v.toAddress, v.cost()
	case sms:
		return v.toPhoneNumber, v.cost()
	default:
		return "", 0.0
	}
}

// don't touch below this line

type expense interface {
	cost() float64
}

type email struct {
	isSubscribed bool
	body         string
	toAddress    string
}

type sms struct {
	isSubscribed  bool
	body          string
	toPhoneNumber string
}

type invalid struct{}

func (em email) cost() float64 {
	if !em.isSubscribed {
		return float64(len(em.body)) * .05
	}
	return float64(len(em.body)) * .01
}

func (sm sms) cost() float64 {
	if !sm.isSubscribed {
		return float64(len(sm.body)) * .1
	}
	return float64(len(sm.body)) * .03
}

func (inv invalid) cost() float64 {
	return 0.0
}
