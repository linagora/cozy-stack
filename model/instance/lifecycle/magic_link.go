package lifecycle

import (
	"errors"
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/emailer"
)

// ErrMagicLinkNotAvailable is used when requesting a magic link on a Cozy
// where this feature has not been activated.
var ErrMagicLinkNotAvailable = errors.New("magic link is not available on this instance")

// ErrInvalidMagicLink is used when the code for a magic link is invalid
var ErrInvalidMagicLink = errors.New("invalid magic link")

func SendMagicLink(inst *instance.Instance, redirect string) error {
	code, err := CreateMagicLinkCode(inst)
	if err != nil {
		return err
	}

	link := inst.PageURL("/auth/magic_link", url.Values{
		"code":     []string{code},
		"redirect": []string{redirect},
	})
	publicName, _ := csettings.PublicName(inst)
	return emailer.SendEmail(inst, &emailer.TransactionalEmailCmd{
		TemplateName: "magic_link",
		TemplateValues: map[string]interface{}{
			"MagicLink":  link,
			"PublicName": publicName,
		},
	})
}

func CreateMagicLinkCode(inst *instance.Instance) (string, error) {
	if !inst.MagicLink {
		return "", ErrMagicLinkNotAvailable
	}

	code := crypto.GenerateRandomString(instance.MagicLinkCodeLen)
	if err := GetStore().SaveMagicLinkCode(inst, code); err != nil {
		return "", err
	}
	return code, nil
}

func CheckMagicLink(inst *instance.Instance, code string) error {
	if !inst.MagicLink || !GetStore().CheckMagicLinkCode(inst, code) {
		return ErrInvalidMagicLink
	}
	return nil
}

// ConsumeMagicLink validates and spends a code once all authentication factors pass.
func ConsumeMagicLink(inst *instance.Instance, code string) error {
	if !inst.MagicLink || !GetStore().ConsumeMagicLinkCode(inst, code) {
		return ErrInvalidMagicLink
	}
	return nil
}
