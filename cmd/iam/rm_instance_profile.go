package cliaws

import (
	"context"

	"github.com/alexflint/go-arg"
	"github.com/nathants/cli-aws/lib"
)

func init() {
	lib.Commands["iam-rm-instance-profile"] = iamRmInstanceProfile
	lib.Args["iam-rm-instance-profile"] = iamRmInstanceProfileArgs{}
}

type iamRmInstanceProfileArgs struct {
	Name    string `arg:"positional,required"`
	Preview bool   `arg:"-p,--preview"`
}

func (iamRmInstanceProfileArgs) Description() string {
	return "\nrm iam instance profile\n"
}

func iamRmInstanceProfile() {
	var args iamRmInstanceProfileArgs
	arg.MustParse(&args)
	ctx := context.Background()
	err := lib.IamDeleteInstanceProfile(ctx, args.Name, args.Preview)
	if err != nil {
		lib.Logger.Fatal("error: ", err)
	}
}
