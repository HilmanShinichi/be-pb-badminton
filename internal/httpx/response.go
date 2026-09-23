package httpx

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"
)

type Envelope struct {
	Data any `json:"data,omitempty"`
	Meta any `json:"meta,omitempty"`
}
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func OK(c *fiber.Ctx, status int, data any) error {
	return c.Status(status).JSON(Envelope{Data: data})
}

func OKWithMeta(c *fiber.Ctx, status int, data, meta any) error {
	return c.Status(status).JSON(Envelope{Data: data, Meta: meta})
}

func Err(c *fiber.Ctx, status int, code, message string) error {
	return c.Status(status).JSON(map[string]any{"error": ErrorBody{Code: code, Message: message}})
}

func WriteAppError(c *fiber.Ctx, err error) error {
	ae := AsAppError(err)
	return Err(c, ae.Status, ae.Code, ae.Message)
}

func Decode(c *fiber.Ctx, into any) error {
	if len(c.Body()) == 0 {
		return errors.New("empty body")
	}
	return c.BodyParser(into)
}

// AppError carries a stable error code from service layer to HTTP status.
type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Message }

func AsAppError(err error) *AppError {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	return &AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Something went wrong on the server."}
}

func BadRequest(code, msg string) error { return &AppError{http.StatusBadRequest, code, msg} }
func Unauthorized(msg string) error     { return &AppError{http.StatusUnauthorized, "UNAUTHORIZED", msg} }
func NotFound(msg string) error         { return &AppError{http.StatusNotFound, "NOT_FOUND", msg} }
func Conflict(code, msg string) error   { return &AppError{http.StatusConflict, code, msg} }
func Unprocessable(msg string) error {
	return &AppError{http.StatusUnprocessableEntity, "VALIDATION_ERROR", msg}
}
