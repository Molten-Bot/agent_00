import React, { useEffect, useState } from "react";
import { AtSign, Building2, FolderGit2, User } from "lucide-react";

function OwnerIcon({ repository }) {
  const [failed, setFailed] = useState(false);
  const avatar = repository.owner_avatar_url || "";
  const type = (repository.owner_type || repository.owner_kind || "").toLowerCase();
  const Icon = type === "organization" ? Building2 : type === "user" ? User : AtSign;
  return /^https?:\/\//.test(avatar) && !failed ? (
    <img className="repository-settings-owner-avatar" src={avatar} alt="" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(true)} />
  ) : <Icon size={18} aria-hidden="true" />;
}

export function RepositoryPicker({ repositories, value, onChange, loading, error, onRetry }) {
  const [owner, setOwner] = useState("");
  useEffect(() => { if (value) setOwner(value.split("/")[0].toLowerCase()); }, [value]);
  const groups = new Map();
  for (const repository of repositories) {
    const name = repository.full_name?.split("/")[0];
    if (!name) continue;
    const key = name.toLowerCase();
    if (!groups.has(key)) groups.set(key, { name, repositories: [] });
    groups.get(key).repositories.push(repository);
  }
  const owners = [...groups.entries()].sort((a, b) => a[1].name.localeCompare(b[1].name));
  const choices = [...(groups.get(owner)?.repositories || [])].sort((a, b) => a.full_name.localeCompare(b.full_name));
  return <div className="wf-repository-picker">
    {loading && <p role="status" className="wf-muted">Loading repositories…</p>}
    {error && <div role="alert"><p className="wf-muted">{error}</p><button type="button" onClick={onRetry}>Retry repositories</button></div>}
    {!loading && !error && !owners.length && <p className="wf-muted">No repositories available. Check repository access in Settings.</p>}
    {!!owners.length && <>
      <div className="repository-settings-column-title" id="wf-owners-label">Organizations &amp; people</div>
      <div className="repository-settings-list wf-picker-list" role="group" aria-labelledby="wf-owners-label">
        {owners.map(([key, group]) => <button key={key} type="button" className="repository-settings-owner" aria-pressed={key === owner} onClick={() => {
          if (key !== owner && onChange("")) setOwner(key);
        }}><OwnerIcon repository={group.repositories[0]} /><span>{group.name}</span><small>{group.repositories.length}</small></button>)}
      </div>
      <div className="repository-settings-column-title" id="wf-repositories-label">Repositories</div>
      <div className="repository-settings-list wf-picker-list" role="group" aria-labelledby="wf-repositories-label">
        {choices.map(repository => <button key={repository.full_name} type="button" className="repository-settings-option" aria-pressed={value === repository.full_name} onClick={() => onChange(repository.full_name)} title={repository.full_name}><FolderGit2 size={18} aria-hidden="true" /><span>{repository.full_name}</span></button>)}
      </div>
      {!owner && <p className="wf-muted">Choose an organization or person.</p>}
      {value && !repositories.some(r => r.full_name === value) && <p role="status" className="wf-muted">{value} is unavailable. Choose a repository from Settings.</p>}
    </>}
  </div>;
}
