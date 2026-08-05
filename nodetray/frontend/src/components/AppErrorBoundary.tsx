import { Component, Fragment, type ReactNode } from 'react'

type AppErrorBoundaryProps = {
  children: ReactNode
}

type AppErrorBoundaryState = {
  failed: boolean
  subtreeKey: number
}

export class AppErrorBoundary extends Component<AppErrorBoundaryProps, AppErrorBoundaryState> {
  state: AppErrorBoundaryState = { failed: false, subtreeKey: 0 }

  private recoveryAttempted = false

  static getDerivedStateFromError(): Partial<AppErrorBoundaryState> {
    return { failed: true }
  }

  componentDidCatch(): void {
    if (this.recoveryAttempted) {
      return
    }
    this.recoveryAttempted = true
    this.setState((state) => ({ failed: false, subtreeKey: state.subtreeKey + 1 }))
  }

  render(): ReactNode {
    if (this.state.failed) {
      return (
        <section role="alert" aria-live="assertive">
          <h1>节点控制台暂时不可用</h1>
          <p>界面恢复失败，请重启托盘程序。</p>
        </section>
      )
    }

    return <Fragment key={this.state.subtreeKey}>{this.props.children}</Fragment>
  }
}
