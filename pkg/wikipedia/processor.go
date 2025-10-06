package wikipedia

import (
	"fmt"
	"log"
	"museum/internal/models"
	"museum/pkg/geo"
	"strings"
)

type CategoryProcessor struct {
	svc       *CategoryService
	extractor *MuseumExtractor
	visited   map[string]struct{}
	total     int
}

func NewCategoryProcessor(svc *CategoryService, extractor *MuseumExtractor) *CategoryProcessor {
	return &CategoryProcessor{
		svc:       svc,
		extractor: extractor,
		visited:   make(map[string]struct{}),
		total:     0,
	}
}
func (p *CategoryProcessor) ProcessCategory(categoryTitle string) {
	p.process(categoryTitle, 0)
}

func (p *CategoryProcessor) process(categoryTitle string, depth int) {
	indent := strings.Repeat("  ", depth)

	if _, seen := p.visited[categoryTitle]; seen {
		fmt.Printf("%s- Skipping already visited category: %s\n", indent, categoryTitle)
		return
	}
	p.visited[categoryTitle] = struct{}{}

	members, err := p.svc.GetAllCategoryMembers(categoryTitle)
	if err != nil {
		log.Printf("Error fetching members for '%s': %v", categoryTitle, err)
		return
	}

	if depth > 0 {
		fmt.Printf("\n%s---------------------------------------------------------------\n", indent)
		fmt.Printf("%sSubcategory: %s\n", indent, categoryTitle)
		fmt.Printf("%s---------------------------------------------------------------\n", indent)
	}

	for _, member := range members {
		if _, seen := p.visited[member.Title]; seen {
			fmt.Printf("%s- Skipping already visited page: %s\n", indent, member.Title)
			continue
		}
		p.visited[member.Title] = struct{}{}

		if member.NS == 14 { // subcategory
			p.process(member.Title, depth+1)
		} else {
			fmt.Printf("%s- Found page: %s\n", indent, member.Title)

			content, err := p.svc.GetPageContent(member.Title)
			if err != nil {
				log.Printf("Error fetching content for '%s': %v", member.Title, err)
				continue
			}

			museums := p.extractor.ExtractMuseums(content)
			for _, museum := range museums {
				if strings.Contains(museum, "List") {
					content, err = p.svc.GetPageContent(member.Title)
					museums = append(museums, p.extractor.ExtractMuseums(content)...)
				} else {
					fmt.Printf("%s  - Museum: %s\n", indent, museum)
				}
			}
			p.total += len(museums)
			fmt.Printf("%s  - Total museums found: %d\n", indent, p.total)
		}
	}
}

func (p *CategoryProcessor) ProcessCategoryAsync(categoryTitle string) <-chan models.Museum {
	out := make(chan models.Museum)
	go func() {
		defer close(out)
		p.processAsync(categoryTitle, out)
	}()
	return out
}

// Internal recursive function streams results to the channel
func (p *CategoryProcessor) processAsync(categoryTitle string, out chan<- models.Museum) {
	if _, seen := p.visited[categoryTitle]; seen {
		return
	}
	p.visited[categoryTitle] = struct{}{}

	members, err := p.svc.GetAllCategoryMembers(categoryTitle)
	if err != nil {
		log.Printf("Error fetching members for '%s': %v", categoryTitle, err)
		return
	}

	for _, member := range members {
		if _, seen := p.visited[member.Title]; seen {
			continue
		}
		p.visited[member.Title] = struct{}{}

		if member.NS == 14 { // subcategory
			p.processAsync(member.Title, out)
		} else { // regular page
			content, err := p.svc.GetPageContent(member.Title)
			if err != nil {
				log.Printf("Error fetching content for '%s': %v", member.Title, err)
				continue
			}
			museums := p.extractor.ExtractMuseums(content)
			for _, museum := range museums {
				if strings.Contains(museum, "List") {
					content, err = p.svc.GetPageContent(member.Title)
					museums = append(museums, p.extractor.ExtractMuseums(content)...)
				} else {
					country := geo.ExtractCountry(member.Title)
					out <- models.Museum{
						Country: country,
						Name:    museum,
					} // stream each museum immediately
				}
			}
		}
	}
}
