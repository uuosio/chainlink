import Card from '@material-ui/core/Card'
import {
  createStyles,
  Theme,
  withStyles,
  WithStyles,
} from '@material-ui/core/styles'
import Typography from '@material-ui/core/Typography'
import React from 'react'

const styles = (theme: Theme) =>
  createStyles({
    style: {
      textAlign: 'center',
      padding: theme.spacing.unit * 2.5,
      position: 'fixed',
      left: '0',
      bottom: '0',
      width: '100%',
    },
    bareAnchor: {
      color: theme.palette.common.black,
      textDecoration: 'none',
    },
  })

interface Props extends WithStyles<typeof styles> {}

const Footnote: React.FC<Props> = ({ classes }) => {
  const backendVersion = `Backend v${__EXPLORER_SERVER_VERSION__}`
  const clientVersion = `Client v${__EXPLORER_CLIENT_VERSION__}`

  return (
    <Card className={classes.style}>
      <Typography inline>
        {backendVersion} {' | '}
      </Typography>
      <Typography inline>
        {clientVersion} {' | '}
      </Typography>
      <a
        target="_blank"
        rel="noopener noreferrer"
        href={`https://github.com/smartcontractkit/chainlink/commit/${__GIT_SHA__}`}
        className={classes.bareAnchor}
      >
        {__GIT_SHA__}
      </a>
      {' | '}
      <a
        target="_blank"
        rel="noopener noreferrer"
        href={`https://github.com/smartcontractkit/chainlink/tree/${__GIT_BRANCH__}`}
        className={classes.bareAnchor}
      >
        {__GIT_BRANCH__}
      </a>
    </Card>
  )
}

export default withStyles(styles)(Footnote)
