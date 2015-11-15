// Package purchase enables purchase of goods and services within Ava.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/avabot/ava/Godeps/_workspace/src/github.com/jmoiron/sqlx"
	"github.com/avabot/ava/shared/auth"
	"github.com/avabot/ava/shared/database"
	"github.com/avabot/ava/shared/datatypes"
	"github.com/avabot/ava/shared/language"
	"github.com/avabot/ava/shared/pkg"
)

type Purchase string

var port = flag.Int("port", 0, "Port used to communicate with Ava.")
var db *sqlx.DB

// resp enables the Run() function to skip to the FollowUp function if basic
// requirements are met.
var resp *datatypes.Response

// State enables the tracking of State within the Purchase package. Users move
// from StateNone to StateComplete as they answer each question Ava poses to
// them.
type State int

const (
	StateNone State = iota
	StatePreferences
	StateBudget
	StateRecommendations
	StateProductSelection
	StateShippingAddress
	StatePurchase
	StateComplete
)

func main() {
	flag.Parse()
	var err error
	db, err = database.connectDB()
	if err != nil {
		log.Fatalln(err)
	}
	trigger := &datatypes.StructuredInput{
		Commands: language.Purchase(),
		Objects:  language.Wines(),
	}
	p, err := pkg.NewPackage("purchase", *port, trigger)
	if err != nil {
		log.Fatalln("creating package", p.Config.Name, err)
	}
	purchase := new(Purchase)
	if err := p.Register(purchase); err != nil {
		log.Fatalln("registering package ", err)
	}
}

func (t *Purchase) Run(m *datatypes.Message,
	respMsg *datatypes.ResponseMsg) error {
	resp = m.NewResponse()
	resp.State = map[string]interface{}{
		"state":            StateNone,             // maintains state
		"query":            "",                    // search query
		"budget":           "",                    // suggested price
		"recommendations":  []datatypes.Product{}, // search results
		"offset":           uint(0),               // index in search
		"shippingAddress":  datatypes.Address,
		"productsSelected": []datatypes.Product{},
	}
	si := m.Input.StructuredInput
	query := ""
	for _, o := range si.Objects {
		query += o + " "
	}
	// request longer query to get more interesting search results
	if len(query) < 10 {
		resp.Sentence = "What do you look for in a wine?"
		resp.State["state"] = StatePreferences
		return pkg.SaveResponse(respMsg, resp)
	}
	// user provided us with a sufficiently detailed query, now search
	currentResp = resp
	return FollowUp(m, respMsg)
}

func (t *Purchase) FollowUp(m *datatypes.Message,
	respMsg *datatypes.ResponseMsg) error {
	if resp == nil {
		if err := m.GetLastResponse(); err != nil {
			return err
		}
		resp = m.LastResponse
	}
	// have we already made the purchase?
	if resp.State["state"].(State) == StateComplete {
		// if so, reset state to allow for other purchases
		return t.Run(m, respMsg)
	}
	// TODO allow the user to direct the conversation, e.g. say "something
	// more expensive" and have Ava respond appropriately

	// if purchase has not been made, do we have all data we need to make a
	// purchase?
	complete, err := updateState()
	if err != nil {
		return err
	}
	// if not, collect all of the necessary data
	for !complete {
		complete, err = updateState()
		if err != nil {
			return err
		}
	}
	err := makePurchase(m.User.ID, resp.State["productsSelected"])
	if err != nil {
		return err
	}
	return pkg.SaveResponse(respMsg, resp)
}

func updateState(m *datatypes.Message, resp *datatypes.Response,
	respMsg *datatypes.ResponseMsg) error {
	switch resp.State["state"].(State) {
	case StatePreferences:
		// TODO ensure Ava remembers past answers for budget
		adjs := language.ExtractAdjectives(
			m.Input.StructuredInput.Objects)
		if len(adjs) == 0 {
			return pkg.SaveResponse(respMsg, resp)
		}
		resp.State["query"].(string) += " " + strings.Join(adjs, " ")
		resp.State["state"] = StateBudget
		resp.Sentence = "How much do you usually pay for a bottle of wine?"
	case StateBudget:
		budget, found := language.ExtractCurrency(m.Input.StructuredInput)
		if !found {
			return pkg.SaveResponse(respMsg, resp)
		}
		resp.State["budget"] = budget
		resp.State["state"] = StateRecommendations
		// skip to StateRecommendations
		return updateState(m, resp, respMsg)
	case StateRecommendations:
		query := resp.State["query"].(string)
		if len(query) == 0 {
			log.Println("err: seeking recommendations without query")
		}
		results, err := search.Find(query, "products/alcohol", 20)
		if err != nil {
			return err
		}
		results = sales.SortByRecommendation(results)
		resp.State["recommendations"] = results
		resp.State["state"] = StateProductSelection
		tmp := fmt.Sprintf("A %s for $%f. Does that sound good?",
			product.Name, float64(product.Price)/100)
		resp.Sentence = language.SuggestedProduct(tmp)
	case StateProductSelection:
		// was the recommendation Ava made good?
		yes, found := language.ExtractYesNo(m.Input.Sentence)
		if !found {
			return handleKeywords(m, respMsg, resp)
		}
		if !yes {
			resp.State["offset"].(uint) += 1
			return pkg.SaveResponse(respMsg, resp)
		}
		selection, err := currentSelection(resp.State)
		if err != nil {
			return err
		}
		resp.State["productsSelected"] = append(
			resp.State["productsSelected"].([]datatypes.Product),
			selection)
	case StateShippingAddress:
		// TODO add memory of shipping addresses
		addr, found := language.ExtractAddress(m.Input.StructuredInput)
		if !found {
			return pkg.SaveResponse(respMsg, resp)
		}
		if err := knowledge.NewAddress(addr); err != nil {
			return err
		}
		resp.State["shippingAddress"] = addr
		resp.State["state"] = StatePurchase
		price := float64(resp.State["price"]) / 100
		tmp := fmt.Sprintf("%s. Should I place the order?", price)
		resp.Sentence = fmt.Sprintf("Ok. It comes to %s", tmp)
	case StatePurchase:
		// TODO ensure Ava follows up to ensure the
		err := makePurchase(m.User.ID, resp.State["productsSelected"])
		if err != nil {
			return err
		}
		resp.State["state"] = StateComplete
		resp.Sentence = "Great! I've placed the order. You'll receive "
		+"a confirmation by email."
	}
	return pkg.SaveResponse(respMsg, resp)
}

func currentSelection(state map[string]interface{}) (int, string, uint64,
	error) {
	recs := state["recommendations"].([]datatypes.Product)
	l := len(recs)
	if l == 0 {
		return "", uint64(0), errors.New("empty recommendations")
	}
	offset := state["offset"].(uint)
	if l <= offset {
		err := errors.New("offset exceeds recommendation length")
		return "", uint64(0), err
	}
	product := recs[offset]
	return product.ID, product.Name, product.Price, nil
}

func makePurchase(uid int, products []datatypes.Product) error {
	err := auth.Purchase(auth.Method, uid, products)
	if err != nil {
		return err
	}
	return nil
}

func handleKeywords(m *datatypes.Message, resp *datatypes.Response,
	respMsg *datatypes.ResponseMsg) error {
	words := strings.Fields(m.Input.Sentence)
	for _, word := range words {
		switch word {
		case "detail", "details", "description", "more about", "review",
			"rating", "rated":
		case "price", "cost", "shipping", "how much":
		case "similar", "else", "different":
			resp.State["offset"].(uint) += 1
		}
	}
	return pkg.SaveResponse(respMsg, resp)
}