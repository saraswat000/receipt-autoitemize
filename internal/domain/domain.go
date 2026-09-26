package domain

import "time"

// ReceiptStatus tracks the uploaded file through OCR.
type ReceiptStatus string

const (
	ReceiptUploaded   ReceiptStatus = "UPLOADED"
	ReceiptProcessing ReceiptStatus = "PROCESSING" // queued or running (async mode)
	ReceiptProcessed  ReceiptStatus = "PROCESSED"
	ReceiptFailed     ReceiptStatus = "OCR_FAILED"
)

// ItemizeStatus says whether the line items can be trusted.
type ItemizeStatus string

const (
	// ItemizeComplete: items + added taxes reconcile with the grand total.
	ItemizeComplete ItemizeStatus = "COMPLETE"
	// ItemizeNeedsReview: the transaction is kept, but a human must look at the items.
	ItemizeNeedsReview ItemizeStatus = "NEEDS_REVIEW"
	// ItemizeFailed: extraction could not produce a usable header (e.g. no total).
	ItemizeFailed ItemizeStatus = "FAILED"
)

// ItemSource records who produced a line item.
type ItemSource string

const (
	SourceAuto ItemSource = "AUTO" // proposed by auto-itemize
	SourceUser ItemSource = "USER" // created or edited through PATCH /items
)

// Receipt is the uploaded file plus its OCR state.
type Receipt struct {
	ID          string        `json:"receipt_id"`
	Filename    string        `json:"filename"`
	ContentType string        `json:"content_type"`
	SizeBytes   int64         `json:"size_bytes"`
	SHA256      string        `json:"sha256"`
	StoragePath string        `json:"-"`
	Status      ReceiptStatus `json:"status"`
	OCRError    *string       `json:"ocr_error"`
	CreatedAt   time.Time     `json:"created_at"`
	ProcessedAt *time.Time    `json:"processed_at"`
}

// OCRResult is one OCR run over a receipt. The latest one is the source for re-itemize.
type OCRResult struct {
	ID        string    `json:"id"`
	ReceiptID string    `json:"receipt_id"`
	Engine    string    `json:"engine"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// TaxLine is one tax on the receipt, stored as its own row.
type TaxLine struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // VAT, GST, ...
	Rate   *Rate  `json:"rate"`
	Amount Money  `json:"amount"`
	// Inclusive is true when the tax is already inside the item prices / total
	// ("incl. VAT 19%"), false when it is added on top ("VAT 19%" after a subtotal).
	Inclusive    bool    `json:"inclusive"`
	Jurisdiction *string `json:"jurisdiction"`
}

// LineItem is one itemized row. Amounts are net of any added-on (exclusive) tax.
type LineItem struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Amount      Money      `json:"amount"`
	Quantity    *float64   `json:"quantity"`
	TaxAmount   *Money     `json:"tax_amount"`
	Source      ItemSource `json:"source"`
}

// Issue explains why itemization is not COMPLETE. Amount fields are set when relevant.
type Issue struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	ItemsTotal    *Money `json:"items_total,omitempty"`
	AddedTaxes    *Money `json:"added_taxes,omitempty"`
	ComputedTotal *Money `json:"computed_total,omitempty"`
	GrandTotal    *Money `json:"grand_total,omitempty"`
	Subtotal      *Money `json:"subtotal,omitempty"`
	Difference    *Money `json:"difference,omitempty"`
}

// Transaction is the expense header created from exactly one receipt.
type Transaction struct {
	ID            string        `json:"id"`
	ReceiptID     string        `json:"receipt_id"`
	OCRResultID   string        `json:"ocr_result_id"`
	Merchant      *string       `json:"merchant"`
	Date          *string       `json:"date"` // ISO 8601 (YYYY-MM-DD)
	Currency      *string       `json:"currency"`
	Subtotal      *Money        `json:"subtotal"`
	GrandTotal    *Money        `json:"grand_total"`
	Taxes         []TaxLine     `json:"taxes"`
	LineItems     []LineItem    `json:"line_items"`
	ItemizeStatus ItemizeStatus `json:"itemize_status"`
	ItemizeIssues []Issue       `json:"itemize_issues"`
	Version       int           `json:"version"` // bumped on every write; exposed as ETag
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}
