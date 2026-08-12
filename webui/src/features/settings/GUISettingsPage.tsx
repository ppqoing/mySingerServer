import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { appApi, GUIConfigRestartError, GUIConfigValidationError, waitForManager, type AppApi } from "../../api/appApi";
import { isAbortError } from "../../api/client";
import type {
  GUIConfig,
  GUIFirstScreenConfig,
  GUIPhase2Config
} from "../../api/contracts";
import "../operational-pages.css";
import "./GUISettingsPage.css";

export interface GUISettingsPageProps {
  readonly api?: AppApi;
  readonly navigate?: (target: string) => void;
}

type LoadState = "loading" | "ready" | "error";

interface FieldProps {
  readonly error?: string;
  readonly label: string;
  readonly path: string;
}

interface TextFieldProps extends FieldProps {
  readonly onChange: (value: string) => void;
  readonly type?: "password" | "text";
  readonly value: string;
}

interface NumberFieldProps extends FieldProps {
  readonly note?: string;
  readonly onChange: (value: number) => void;
  readonly step?: number;
  readonly value: number;
}

function fieldID(path: string): string {
  return `gui-config-${path.replace(/[^a-z0-9]+/gi, "-")}`;
}

function TextField({ error, label, onChange, path, type = "text", value }: TextFieldProps) {
  const id = fieldID(path);
  const errorID = `${id}-error`;
  return (
    <div className="gui-settings__field">
      <label htmlFor={id}>{label}</label>
      <input
        aria-describedby={error ? errorID : undefined}
        aria-invalid={error ? "true" : undefined}
        id={id}
        name={path}
        onChange={event => onChange(event.currentTarget.value)}
        type={type}
        value={value}
      />
      {error ? <p className="gui-settings__field-error" id={errorID} role="alert">{error}</p> : null}
    </div>
  );
}

function NumberField({ error, label, note, onChange, path, step = 1, value }: NumberFieldProps) {
  const id = fieldID(path);
  const errorID = `${id}-error`;
  const noteID = `${id}-note`;
  const describedBy = [note ? noteID : undefined, error ? errorID : undefined].filter(Boolean).join(" ") || undefined;
  return (
    <div className="gui-settings__field">
      <label htmlFor={id}>{label}</label>
      <input
        aria-describedby={describedBy}
        aria-invalid={error ? "true" : undefined}
        id={id}
        name={path}
        onChange={event => onChange(Number(event.currentTarget.value))}
        step={step}
        type="number"
        value={value}
      />
      {note ? <p className="gui-settings__field-note" id={noteID}>{note}</p> : null}
      {error ? <p className="gui-settings__field-error" id={errorID} role="alert">{error}</p> : null}
    </div>
  );
}

export function GUISettingsPage({ api = appApi, navigate = target => window.location.assign(target) }: GUISettingsPageProps) {
  const [config, setConfig] = useState<GUIConfig>();
  const [baseline, setBaseline] = useState<GUIConfig>();
  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [saving, setSaving] = useState(false);
  const [showDSN, setShowDSN] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<string>();
  const [pageError, setPageError] = useState<string>();
  const [diskRestartRequired, setDiskRestartRequired] = useState(false);
  const loadController = useRef<AbortController | null>(null);
  const saveController = useRef<AbortController | null>(null);

  const loadConfig = useCallback(async () => {
    loadController.current?.abort();
    const controller = new AbortController();
    loadController.current = controller;
    setLoadState("loading");
    setPageError(undefined);
    setNotice(undefined);
    setFieldErrors({});
    try {
      const snapshot = await api.loadGUIConfig(controller.signal);
      if (controller.signal.aborted) return;
      setConfig(snapshot.config);
      setBaseline(snapshot.config);
      setDiskRestartRequired(snapshot.restartRequired);
      setLoadState("ready");
    } catch (error) {
      if (isAbortError(error) || controller.signal.aborted) return;
      setPageError(error instanceof Error ? error.message : "读取配置失败")
      setLoadState("error");
    }
  }, [api]);

  useEffect(() => {
    const controller = new AbortController();
    loadController.current = controller;
    void api.loadGUIConfig(controller.signal).then(snapshot => {
      if (controller.signal.aborted) return;
      setConfig(snapshot.config);
      setBaseline(snapshot.config);
      setDiskRestartRequired(snapshot.restartRequired);
      setLoadState("ready");
    }).catch(error => {
      if (isAbortError(error) || controller.signal.aborted) return;
      setPageError(error instanceof Error ? error.message : "读取配置失败");
      setLoadState("error");
    });
    return () => {
      loadController.current?.abort();
      saveController.current?.abort();
    };
  }, [api]);

  const dirty = useMemo(
    () => config !== undefined && baseline !== undefined && JSON.stringify(config) !== JSON.stringify(baseline),
    [baseline, config]
  );

  const beginEdit = () => {
    setFieldErrors({});
    setNotice(undefined);
  };

  const updateTopLevel = <Key extends "listenAddr" | "pgDsn" | "heartbeatS">(
    key: Key,
    value: GUIConfig[Key]
  ) => {
    beginEdit();
    setConfig(current => current ? { ...current, [key]: value } : current);
  };

  const updateFirstScreen = <Key extends keyof GUIFirstScreenConfig>(
    key: Key,
    value: GUIFirstScreenConfig[Key]
  ) => {
    beginEdit();
    setConfig(current => current ? {
      ...current,
      firstScreen: { ...current.firstScreen, [key]: value }
    } : current);
  };

  const updatePhase2 = <Key extends keyof GUIPhase2Config>(
    key: Key,
    value: GUIPhase2Config[Key]
  ) => {
    beginEdit();
    setConfig(current => current ? {
      ...current,
      phase2: { ...current.phase2, [key]: value }
    } : current);
  };

  const updateAgent = (index: number, value: string) => {
    beginEdit();
    setConfig(current => {
      if (!current) return current;
      const agents = current.agents.map((agent, agentIndex) => agentIndex === index
        ? { ...agent, addr: value }
        : agent);
      return { ...current, agents };
    });
  };

  const addAgent = () => {
    beginEdit();
    setConfig(current => current ? {
      ...current,
      agents: [...current.agents, { addr: "" }]
    } : current);
  };

  const removeAgent = (index: number) => {
    beginEdit();
    setConfig(current => !current || current.agents.length <= 1
      ? current
      : { ...current, agents: current.agents.filter((_, agentIndex) => agentIndex !== index) });
  };

  const moveAgent = (index: number, direction: -1 | 1) => {
    beginEdit();
    setConfig(current => {
      if (!current) return current;
      const destination = index + direction;
      if (destination < 0 || destination >= current.agents.length) return current;
      const agents = [...current.agents];
      [agents[index], agents[destination]] = [agents[destination], agents[index]];
      return { ...current, agents };
    });
  };

  const saveConfig = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!config || saving) return;
    saveController.current?.abort();
    const controller = new AbortController();
    saveController.current = controller;
    setSaving(true);
    setPageError(undefined);
    setNotice(undefined);
    setFieldErrors({});
    try {
      const result = await api.saveGUIConfig(config, controller.signal);
      if (controller.signal.aborted) return;
      setBaseline(config);
      setDiskRestartRequired(result.restartRequired);
      if (result.restarting) {
        setNotice("配置已保存，Manager 正在自动重启");
        await waitForManager(result.recoveryURL, controller.signal);
        if (!controller.signal.aborted) navigate(`${new URL(result.recoveryURL).origin}/#/settings`);
      } else {
        setNotice(!result.saved ? "配置未变化" : result.restartRequired
          ? "配置已保存，请手动重启 GUI 后生效"
          : "配置已保存，当前无需重启");
      }
    } catch (error) {
      if (isAbortError(error) || controller.signal.aborted) return;
      if (error instanceof GUIConfigRestartError) {
        setBaseline(config);
        setDiskRestartRequired(error.restartRequired);
        setNotice(error.saved
          ? "配置已保存，但自动重启失败，请检查 data\\logs\\gui.log"
          : "自动重启失败，请检查 data\\logs\\gui.log");
      } else if (error instanceof GUIConfigValidationError) {
        setFieldErrors(Object.fromEntries(error.fields.map(field => [field.field, field.message])));
      } else {
        setPageError(error instanceof Error && error.message === "Manager restart timed out"
          ? "重启后监听失败，请检查 data\\logs\\gui.log"
          : error instanceof Error ? error.message : "保存配置失败");
      }
    } finally {
      if (!controller.signal.aborted) setSaving(false);
    }
  };

  return (
    <section aria-labelledby="gui-settings-heading" className="operational-page gui-settings">
      <header className="operational-page__header operational-surface">
        <h1 id="gui-settings-heading">GUI 设置</h1>
        <p>编辑完整 GUI 配置；变更监听地址或连接参数时，Manager 会自动重启并恢复到此页面。</p>
        {dirty ? <strong className="gui-settings__dirty">有未保存更改</strong> : null}
        {!dirty && diskRestartRequired ? <strong className="gui-settings__restart">磁盘配置等待重启生效</strong> : null}
      </header>

      {loadState === "loading" ? <section className="operational-surface"><p>正在读取配置…</p></section> : null}
      {loadState === "error" ? (
        <section className="operational-surface">
          <p role="alert">{pageError || "读取配置失败"}</p>
          <button onClick={() => void loadConfig()} type="button">重新加载</button>
        </section>
      ) : null}

      {loadState === "ready" && config ? (
        <form className="gui-settings__form" onSubmit={saveConfig}>
          <section className="operational-surface gui-settings__section">
            <h2>基本设置</h2>
            <div className="gui-settings__grid">
              <TextField
                error={fieldErrors.listen_addr}
                label="监听地址"
                onChange={value => updateTopLevel("listenAddr", value)}
                path="listen_addr"
                value={config.listenAddr}
              />
              <NumberField
                error={fieldErrors.heartbeat_s}
                label="心跳间隔（秒）"
                onChange={value => updateTopLevel("heartbeatS", value)}
                path="heartbeat_s"
                value={config.heartbeatS}
              />
            </div>
          </section>

          <section className="operational-surface gui-settings__section">
            <h2>PostgreSQL</h2>
            <div className="gui-settings__dsn-row">
              <TextField
                error={fieldErrors.pg_dsn}
                label="PostgreSQL DSN"
                onChange={value => updateTopLevel("pgDsn", value)}
                path="pg_dsn"
                type={showDSN ? "text" : "password"}
                value={config.pgDsn}
              />
              <button className="gui-settings__secondary" onClick={() => setShowDSN(value => !value)} type="button">
                {showDSN ? "隐藏 DSN" : "显示 DSN"}
              </button>
            </div>
          </section>

          <section className="operational-surface gui-settings__section">
            <div className="gui-settings__section-heading">
              <div>
                <h2>Agent</h2>
                <p>列表保存后在 GUI 重启时重建连接池。</p>
              </div>
              <button onClick={addAgent} type="button">添加 Agent</button>
            </div>
            <div className="gui-settings__agents">
              {config.agents.map((agent, index) => (
                <fieldset className="gui-settings__agent" key={index}>
                  <legend>Agent {index + 1}</legend>
                  <TextField
                    error={fieldErrors[`agents[${index}].addr`]}
                    label={`Agent 地址 ${index + 1}`}
                    onChange={value => updateAgent(index, value)}
                    path={`agents[${index}].addr`}
                    value={agent.addr}
                  />
                  <div className="gui-settings__agent-actions">
                    <button
                      aria-label={`上移 Agent ${index + 1}`}
                      className="gui-settings__secondary"
                      disabled={index === 0}
                      onClick={() => moveAgent(index, -1)}
                      type="button"
                    >上移</button>
                    <button
                      aria-label={`下移 Agent ${index + 1}`}
                      className="gui-settings__secondary"
                      disabled={index === config.agents.length - 1}
                      onClick={() => moveAgent(index, 1)}
                      type="button"
                    >下移</button>
                    <button
                      aria-label={`删除 Agent ${index + 1}`}
                      className="gui-settings__danger"
                      disabled={config.agents.length === 1}
                      onClick={() => removeAgent(index)}
                      type="button"
                    >删除</button>
                  </div>
                </fieldset>
              ))}
            </div>
            {fieldErrors.agents ? <p className="gui-settings__field-error" role="alert">{fieldErrors.agents}</p> : null}
            {config.agents.length === 1 ? <p className="gui-settings__field-note">至少需要一个 Agent</p> : null}
          </section>

          <section className="operational-surface gui-settings__section">
            <h2>一筛参数</h2>
            <div className="gui-settings__grid">
              <NumberField error={fieldErrors["firstscreen.hamming_max"]} label="一筛汉明距离上限" onChange={value => updateFirstScreen("hammingMax", value)} path="firstscreen.hamming_max" value={config.firstScreen.hammingMax} />
              <NumberField error={fieldErrors["firstscreen.aspect_tolerance"]} label="一筛宽高比容差" onChange={value => updateFirstScreen("aspectTolerance", value)} path="firstscreen.aspect_tolerance" step={0.01} value={config.firstScreen.aspectTolerance} />
              <NumberField error={fieldErrors["firstscreen.video_duration_window_ms"]} label="视频时长窗口（毫秒）" onChange={value => updateFirstScreen("videoDurationWindowMs", value)} path="firstscreen.video_duration_window_ms" value={config.firstScreen.videoDurationWindowMs} />
              <NumberField error={fieldErrors["firstscreen.image_quality_min"]} label="图片质量下限" onChange={value => updateFirstScreen("imageQualityMin", value)} path="firstscreen.image_quality_min" value={config.firstScreen.imageQualityMin} />
              <NumberField error={fieldErrors["firstscreen.read_page_size"]} label="一筛读取页大小" onChange={value => updateFirstScreen("readPageSize", value)} path="firstscreen.read_page_size" value={config.firstScreen.readPageSize} />
              <NumberField error={fieldErrors["firstscreen.group_insert_batch"]} label="分组写入批次" onChange={value => updateFirstScreen("groupInsertBatch", value)} path="firstscreen.group_insert_batch" value={config.firstScreen.groupInsertBatch} />
              <NumberField error={fieldErrors["firstscreen.sha_resolve_chunk"]} label="SHA 解析分块" onChange={value => updateFirstScreen("shaResolveChunk", value)} path="firstscreen.sha_resolve_chunk" value={config.firstScreen.shaResolveChunk} />
            </div>
          </section>

          <section className="operational-surface gui-settings__section">
            <h2>二筛参数</h2>
            <div className="gui-settings__grid">
              <NumberField error={fieldErrors["phase2.phash_pass_t2"]} label="二筛 PHash 通过阈值" onChange={value => updatePhase2("phashPassT2", value)} path="phase2.phash_pass_t2" step={0.01} value={config.phase2.phashPassT2} />
              <NumberField error={fieldErrors["phase2.phash_part_threshold"]} label="二筛 PHash 分块阈值" onChange={value => updatePhase2("phashPartThreshold", value)} path="phase2.phash_part_threshold" value={config.phase2.phashPartThreshold} />
              <NumberField error={fieldErrors["phase2.sobel_t3"]} label="二筛 Sobel 阈值" onChange={value => updatePhase2("sobelT3", value)} path="phase2.sobel_t3" step={0.01} value={config.phase2.sobelT3} />
              <NumberField error={fieldErrors["phase2.video_frames"]} label="视频抽帧数" note="当前必须为 6" onChange={value => updatePhase2("videoFrames", value)} path="phase2.video_frames" value={config.phase2.videoFrames} />
              <NumberField error={fieldErrors["phase2.video_avg_t4"]} label="视频平均阈值" onChange={value => updatePhase2("videoAvgT4", value)} path="phase2.video_avg_t4" step={0.01} value={config.phase2.videoAvgT4} />
              <NumberField error={fieldErrors["phase2.video_min_passed"]} label="视频最少通过帧数" onChange={value => updatePhase2("videoMinPassed", value)} path="phase2.video_min_passed" value={config.phase2.videoMinPassed} />
              <NumberField error={fieldErrors["phase2.video_min_valid"]} label="视频最少有效帧数" onChange={value => updatePhase2("videoMinValid", value)} path="phase2.video_min_valid" value={config.phase2.videoMinValid} />
              <NumberField error={fieldErrors["phase2.video_file_timeout_s"]} label="视频文件超时（秒）" onChange={value => updatePhase2("videoFileTimeoutS", value)} path="phase2.video_file_timeout_s" value={config.phase2.videoFileTimeoutS} />
              <NumberField error={fieldErrors["phase2.video_frame_command_timeout_s"]} label="单帧命令超时（秒）" onChange={value => updatePhase2("videoFrameCommandTimeoutS", value)} path="phase2.video_frame_command_timeout_s" value={config.phase2.videoFrameCommandTimeoutS} />
              <NumberField error={fieldErrors["phase2.image_file_timeout_s"]} label="图片文件超时（秒）" onChange={value => updatePhase2("imageFileTimeoutS", value)} path="phase2.image_file_timeout_s" value={config.phase2.imageFileTimeoutS} />
              <NumberField error={fieldErrors["phase2.task_shard_size"]} label="二筛任务分片大小" onChange={value => updatePhase2("taskShardSize", value)} path="phase2.task_shard_size" value={config.phase2.taskShardSize} />
              <div className="gui-settings__field gui-settings__checkbox">
                <label htmlFor="gui-config-phase2-auto-dispatch">
                  <input
                    checked={config.phase2.autoDispatch}
                    id="gui-config-phase2-auto-dispatch"
                    name="phase2.auto_dispatch"
                    onChange={event => updatePhase2("autoDispatch", event.currentTarget.checked)}
                    type="checkbox"
                  />
                  自动分发二筛任务
                </label>
                {fieldErrors["phase2.auto_dispatch"] ? <p className="gui-settings__field-error" role="alert">{fieldErrors["phase2.auto_dispatch"]}</p> : null}
              </div>
            </div>
          </section>

          <section className="operational-surface gui-settings__actions">
            <div>
              {notice ? <p className="gui-settings__notice" role="status">{notice}</p> : null}
              {pageError ? <p className="gui-settings__field-error" role="alert">{pageError}</p> : null}
            </div>
            <button className="gui-settings__secondary" disabled={saving} onClick={() => void loadConfig()} type="button">重新加载</button>
            <button disabled={saving} type="submit">{saving ? "正在保存…" : "保存配置"}</button>
          </section>
        </form>
      ) : null}
    </section>
  );
}
