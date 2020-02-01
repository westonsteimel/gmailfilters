package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/gmail/v1"
)

// filterfile defines a set of filter objects.
type filterfile struct {
	Filter []filter
}

// filter defines a filter object.
type filter struct {
	Query             string
	NegatedQuery      string
	Archive           bool
	ArchiveUnlessToMe bool
	Read              bool
	Delete            bool
	Important         bool
	Star              bool
	Spam              bool
	Labels            []string
	ForwardTo         string
}

func (f filter) toGmailFilters(labels *labelMap) ([]gmail.Filter, error) {
	// Convert the filter into a gmail filter.

	if len(f.Query) < 1 && len(f.NegatedQuery) < 1 {
		return nil, errors.New("Query and NegatedQuery cannot both be empty")
	}

	if f.Archive && f.ArchiveUnlessToMe {
		return nil, errors.New("Archive and ArchiveUnlessToMe cannot both be true")
	}

	action := gmail.FilterAction{
		AddLabelIds:    []string{},
		RemoveLabelIds: []string{},
	}

	if f.Archive && !f.ArchiveUnlessToMe {
		action.RemoveLabelIds = append(action.RemoveLabelIds, "INBOX")
	}

	if f.Read {
		action.RemoveLabelIds = append(action.RemoveLabelIds, "UNREAD")
	}

	if f.Delete {
		action.AddLabelIds = append(action.AddLabelIds, "TRASH")
	}

	if f.Important {
		action.AddLabelIds = append(action.AddLabelIds, "IMPORTANT")
	}

	if f.Star {
		action.AddLabelIds = append(action.AddLabelIds, "STARRED")
	}

	if f.Spam {
		action.AddLabelIds = append(action.AddLabelIds, "SPAM")
	}

	if len(f.ForwardTo) > 0 {
		action.Forward = f.ForwardTo
	}

	criteria := gmail.FilterCriteria{
		Query:        f.Query,
		NegatedQuery: f.NegatedQuery,
	}

	if f.ArchiveUnlessToMe {
		criteria.To = "me"
	}

	filter := gmail.Filter{
		Action:   &action,
		Criteria: &criteria,
	}

	filters := []gmail.Filter{}

	// If we need to archive unless to them, then add the additional filter.
	if f.ArchiveUnlessToMe {
		// Copy the filter.
		archiveIfNotToMeFilter := filter
		archiveIfNotToMeFilter.Criteria = &gmail.FilterCriteria{
			Query:        f.Query,
			To:           "(-me)",
			NegatedQuery: f.NegatedQuery,
		}

		// Copy the action.
		archiveAction := action
		// Archive it.
		archiveAction.RemoveLabelIds = append(action.RemoveLabelIds, "INBOX")
		archiveIfNotToMeFilter.Action = &archiveAction

		// Append the extra filter.
		filters = append(filters, archiveIfNotToMeFilter)
	}

	if len(f.Labels) > 1 {
		// Create a rule per label with only the query criteria
		for _, label := range f.Labels {
			labelID, err := labels.createLabelIfDoesNotExist(label)
			if err != nil {
				return nil, err
			}

			// We can only add a single user label per filter, so clone and
			// create a new filter and action per label
			labelAction := gmail.FilterAction{
				AddLabelIds: []string{
					labelID,
				},
			}

			labelCriteria := gmail.FilterCriteria{
				Query:        f.Query,
				NegatedQuery: f.NegatedQuery,
			}

			labelFilter := gmail.Filter{
				Action:   &labelAction,
				Criteria: &labelCriteria,
			}

			filters = append(filters, labelFilter)
		}
	} else if len(f.Labels) == 1 {
		labelID, err := labels.createLabelIfDoesNotExist(f.Labels[0])
		if err != nil {
			return nil, err
		}

		action.AddLabelIds = append(action.AddLabelIds, labelID)
	}

	if len(action.AddLabelIds) > 0 || len(action.RemoveLabelIds) > 0 {
		filters = append(filters, filter)
	}

	return filters, nil
}

func (f filter) addFilter(labels *labelMap) error {
	// Convert the filter into a gmail filter.
	filters, err := f.toGmailFilters(labels)
	if err != nil {
		return err
	}

	// Add the filters.
	for _, fltr := range filters {
		logrus.WithFields(logrus.Fields{
			"action":   fmt.Sprintf("%#v", fltr.Action),
			"criteria": fmt.Sprintf("%#v", fltr.Criteria),
		}).Debug("adding Gmail filter")
		if _, err := api.Users.Settings.Filters.Create(gmailUser, &fltr).Do(); err != nil {
			return fmt.Errorf("creating filter [%#v] failed: %v", fltr, err)
		}
	}

	return nil
}

func decodeFile(file string) ([]filter, error) {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading filter file %s failed: %v", file, err)
	}

	var ff filterfile
	if _, err := toml.Decode(string(b), &ff); err != nil {
		return nil, fmt.Errorf("decoding toml failed: %v", err)
	}

	return ff.Filter, nil
}

func exportExistingFilters(file string) error {
	fmt.Print("exporting existing filters...\n")

	filters, err := getExistingFilters()
	if err != nil {
		return fmt.Errorf("error downloading existing filters: %v", err)
	}

	var ff filterfile
	for _, f := range filters {
		existingFilter := findExistingFilter(&ff.Filter, f)

		// Since we can't return nil on a struct or compare it to something empty,
		// check if the query exists. If not then consider it not found.
		if existingFilter.Query != "" || existingFilter.NegatedQuery != "" {
			if len(f.Labels) > 0 {
				existingFilter.Labels = append(existingFilter.Labels, f.Labels...)
			}

			if f.ArchiveUnlessToMe {
				existingFilter.ArchiveUnlessToMe = true
			}

			logrus.WithFields(logrus.Fields{
				"Query":          fmt.Sprintf("%#v", f.Query),
				"IncomingLabels": fmt.Sprintf("%#v", f.Labels),
				"UpdatedLabels":  fmt.Sprintf("%#v", existingFilter.Labels),
			}).Debug("existing Filter update")
		} else {
			logrus.WithFields(logrus.Fields{
				"Labels": fmt.Sprintf("%#v", f.Labels),
			}).Debug("new exported filter")

			ff.Filter = append(ff.Filter, f)
		}
	}

	return writeFiltersToFile(ff, file)
}

func deleteExistingFilters() error {
	// Get current filters for the user.
	l, err := api.Users.Settings.Filters.List(gmailUser).Do()
	if err != nil {
		return fmt.Errorf("listing filters failed: %v", err)
	}

	// Iterate over the filters.
	for _, f := range l.Filter {
		// Delete the filter.
		if err := api.Users.Settings.Filters.Delete(gmailUser, f.Id).Do(); err != nil {
			return fmt.Errorf("deleting filter id %s failed: %v", f.Id, err)
		}
	}

	return nil
}

func getExistingFilters() ([]filter, error) {
	gmailFilters, err := api.Users.Settings.Filters.List(gmailUser).Do()
	if err != nil {
		return nil, err
	}

	labels, err := getLabelMapOnID()
	if err != nil {
		return nil, err
	}

	var filters []filter
	fmt.Println(len(gmailFilters.Filter))
	for _, gmailFilter := range gmailFilters.Filter {
		f := filter{
			Labels: []string{},
		}

		fmt.Println(gmailFilter.Criteria.Query)

		if gmailFilter.Criteria.Query > "" {
			f.Query = gmailFilter.Criteria.Query
		}

		if gmailFilter.Criteria.NegatedQuery > "" {
			f.NegatedQuery = gmailFilter.Criteria.NegatedQuery
		}

		if len(gmailFilter.Action.AddLabelIds) > 0 {
			for _, labelID := range gmailFilter.Action.AddLabelIds {
				if labelID == "TRASH" {
					f.Delete = true
				} else if labelID == "IMPORTANT" {
					f.Important = true
				} else if labelID == "STARRED" {
					f.Star = true
				} else if labelID == "SPAM" {
					f.Spam = true
				} else {
					labelName, ok := labels[labelID]
					if ok {
						f.Labels = append(f.Labels, labelName)
					}
				}
			}
		}

		if len(gmailFilter.Action.RemoveLabelIds) > 0 {
			for _, labelID := range gmailFilter.Action.RemoveLabelIds {
				if labelID == "UNREAD" {
					f.Read = true
				} else if labelID == "INBOX" {
					if gmailFilter.Criteria.To == "me" || gmailFilter.Criteria.To == "(-me)" {
						f.ArchiveUnlessToMe = true
						f.Archive = false
					} else {
						f.Archive = true
					}
				}
			}
		}

		filters = append(filters, f)
	}

	return filters, nil
}

func writeFiltersToFile(ff filterfile, file string) error {
	exportFile, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("error exporting filters: %v", err)
	}

	writer := bufio.NewWriter(exportFile)
	encoder := toml.NewEncoder(writer)
	encoder.Indent = ""

	if err := encoder.Encode(ff); err != nil {
		return fmt.Errorf("error writing file: %v", err)
	}

	fmt.Printf("Exported %d filters\n", len(ff.Filter))

	return nil
}

func findExistingFilter(existingFilters *[]filter, compFilter filter) *filter {
	for _, f := range *existingFilters {
		if f.Query == compFilter.Query && f.NegatedQuery == compFilter.NegatedQuery {
			return &f
		}
	}

	return &filter{
		Labels: []string{},
	}
}
