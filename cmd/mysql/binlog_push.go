package mysql

import (
	"github.com/spf13/cobra"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/internal/databases/mysql"
)

const binlogPushShortDescription = "Upload binlogs to the storage"

var untilBinlog string

// binlogPushCmd represents the cron command
var binlogPushCmd = &cobra.Command{
	Use:   "binlog-push",
	Short: binlogPushShortDescription,
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		uploader, err := internal.ConfigureUploader()
		tracelog.ErrorLogger.FatalOnError(err)
		checkGTIDs, _ := conf.GetBoolSettingDefault(conf.MysqlCheckGTIDs, false)
		mysql.HandleBinlogPush(uploader, untilBinlog, checkGTIDs)
	},
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.RequiredSettings[conf.MysqlDatasourceNameSetting] = true
		err := internal.AssertRequiredSettingsSet()
		tracelog.ErrorLogger.FatalOnError(err)
	},
}

func init() {
	cmd.AddCommand(binlogPushCmd)
	binlogPushCmd.Flags().StringVar(&untilBinlog, "until", "", "binlog file name to stop at. Current active by default")
}
