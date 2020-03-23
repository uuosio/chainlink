import Grid from '@material-ui/core/Grid'
import { createStyles, withStyles, WithStyles } from '@material-ui/core/styles'
import { RouteComponentProps } from '@reach/router'
import React from 'react'
import Footer from '../components/Admin/Operators/Footer'

const styles = () =>
  createStyles({
    container: {
      overflow: 'hidden',
    },
  })

interface Props extends RouteComponentProps, WithStyles<typeof styles> {}

export const AdminMinimal: React.FC<Props> = ({ children, classes }) => {
  return (
    <>
      <Grid
        container
        spacing={24}
        alignItems="center"
        className={classes.container}
      >
        <Grid item xs={12}>
          <main>{children}</main>
        </Grid>
      </Grid>
      <Footer />
    </>
  )
}

export default withStyles(styles)(AdminMinimal)
