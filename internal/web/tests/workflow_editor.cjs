const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {tmpdir}=require('node:os');
const {join}=require('node:path');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
const defaultRepos={ok:true,repos:[{full_name:'acme/repo'},{full_name:'acme/other'},{full_name:'person/project',owner_type:'User'}]};
async function openEditor(t,viewport={width:1280,height:900},initialSettings={workflow_enabled:true},getAgentAuth=()=>({required:false,ready:true,state:'ready'}),mockStorage=true,getRepos=()=>defaultRepos){
 let settings=initialSettings;
 const page=await browser.newPage({viewport});const errors=[];page.on('pageerror',e=>errors.push(e.message));t.after(async()=>{await page.close();assert.deepEqual(errors,[])});
 let doc={version:2,revision:0,workflows:[]};
 let beforeSave;
 await page.addInitScript(()=>{window.EventSource=class{}});
 await page.route(url=>mockStorage || url.pathname!=='/api/workflows',async route=>{const url=new URL(route.request().url());if(url.origin!==new URL(baseURL).origin)return route.abort();
 if(url.pathname==='/api/workflows'){if(route.request().method()==='PUT'){const next=route.request().postDataJSON();await beforeSave?.(next);if(next.revision!==doc.revision)return route.fulfill({status:409,json:{error:'workflows changed; reload before saving'}});doc={...next,revision:doc.revision+1}}return route.fulfill({json:doc})}
 if(url.pathname==='/api/workflows/preview')return route.fulfill({json:[{item:{number:12,title:'Example issue'},matches:true,reason:'Trigger and filters match'}]});
 if(url.pathname==='/api/library')return route.fulfill({json:{tasks:[{name:'audit',displayName:'Audit repository',prompt:'Audit the repository.'}]}});
 if(url.pathname==='/api/workflows/schedules')return route.fulfill({json:[]});
 if(url.pathname==='/api/github/repos')return route.fulfill({status:200,json:getRepos(url)});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:await getAgentAuth()}});
 if(url.pathname==='/api/review-settings'){if(route.request().method()==='POST')settings=route.request().postDataJSON();return route.fulfill({json:{ok:true,settings}})};
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#workflow');await page.locator('#workflow-title').waitFor();return {page,getDocument:()=>doc,setDocument:value=>{doc=value},onSave:handler=>{beforeSave=handler}};
}
async function waitForSaved(page) {
 await page.locator('.wf-save-state:empty').waitFor({state:'attached'});
}
test('default execution remains editable with custom workflows disabled', async t => {
 const {page}=await openEditor(t,undefined,{workflow_enabled:false},undefined,false);
 const read=async()=>(await page.request.get(baseURL+'/api/workflows')).json();
 const before=await read();
 try {
  await page.getByText('Default execution',{exact:true}).click();
  await page.getByLabel('Default review fix passes').fill('10');
  await page.getByLabel('Default reviewer complexity').selectOption('easy');
  const saved=page.waitForResponse(response=>response.url().endsWith('/api/workflows')&&response.request().method()==='PUT');
  await page.getByRole('button',{name:'Save default execution'}).click();
  assert.equal((await saved).status(),200);
  assert.deepEqual((await read()).defaultExecution,{reviewCycles:10,reviewAgent:{complexity:'easy'}});
  await page.reload();
  await page.getByText('Default execution',{exact:true}).click();
  assert.equal(await page.getByLabel('Default review fix passes').inputValue(),'10');
  assert.equal(await page.getByLabel('Default reviewer complexity').inputValue(),'easy');
 } finally {
  const latest=await read();
  await page.request.put(baseURL+'/api/workflows',{data:{...latest,defaultExecution:before.defaultExecution}});
 }
});
async function selectRepository(page, repository) {
 const owner = repository.split('/')[0];
 const owners = page.getByRole('group', {name:'Organizations & people',exact:true});
 const button = owners.getByRole('button').filter({hasText:owner});
 if (await button.getAttribute('aria-pressed') !== 'true') await button.click();
 await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button',{name:repository,exact:true}).click();
}
async function visibleNodeCenter(page,node) {
 await node.hover();
 const bounds=await node.boundingBox();
 const canvas=await page.locator('.react-flow').boundingBox();
 const viewport=await page.evaluate(()=>({width:innerWidth,height:innerHeight}));
 const left=Math.max(bounds.x,canvas.x,0),right=Math.min(bounds.x+bounds.width,canvas.x+canvas.width,viewport.width);
 const top=Math.max(bounds.y,canvas.y,0),bottom=Math.min(bounds.y+bounds.height,canvas.y+canvas.height,viewport.height);
 assert.ok(right>left&&bottom>top,'node and canvas overlap inside the viewport');
 return {x:(left+right)/2,y:(top+bottom)/2};
}
test('Repository settings show an icon-only GitHub link for the workflow repository', async t => {
 const {page}=await openEditor(t);
 await selectRepository(page,'acme/other');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.locator('.react-flow__node[data-id="repository"]').click();
 const panel=page.getByRole('complementary',{name:'Step settings'});
 const link=panel.getByRole('link',{name:'Open acme/other on GitHub',exact:true});
 await link.waitFor();
 assert.equal(await link.getAttribute('href'),'https://github.com/acme/other');
 assert.equal(await link.getAttribute('target'),'_blank');
 assert.equal((await link.textContent()).trim(),'');
 await link.locator('img[src="/static/logos/github.svg"]').evaluate(img=>img.decode());
 assert.equal(await panel.getByText('The repository filter and commit signing settings apply to every workflow.',{exact:true}).count(),0);
});
test('Agent task updates its SVG when the active harness changes',async t=>{
 let finishAuth;
 const auth=new Promise(resolve=>{finishAuth=resolve});
 const ready={required:false,ready:true,state:'ready'};
 const claude={...ready,harness:'claude'};
 t.after(()=>finishAuth(claude));
 let authRequests=0;
 const {page}=await openEditor(t,undefined,undefined,()=>authRequests++===0 ? {...ready,harness:'codex'} : auth);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByLabel('Workflow name').fill('Keep this draft');
 const logo=page.locator('[data-id="task"] .wf-icon img');
 const expectLogo=async name=>{
  const src=`/static/logos/${name}.svg`;
  await logo.and(page.locator(`img[src="${src}"]`)).waitFor();
  await logo.evaluate(img=>img.decode());
 };
 await expectLogo('codex-cli');
 finishAuth(claude);
 await expectLogo('claude-code');
 assert.equal(await page.getByLabel('Workflow name').inputValue(),'Keep this draft');
});
test('multiple persistent workflows, disabled drafts, policy editing, activation and pause',async t=>{const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.locator('.wf-node').first().waitFor();assert.equal(await page.locator('.wf-node').count(),4);
 await page.getByLabel('Workflow name').fill('Implement ready issues');await waitForSaved(page);assert.equal(getDocument().workflows[0].enabled,false);
 const pausedCard=page.getByRole('button',{name:'Implement ready issues, acme/repo, Paused',exact:true});
 const runningCard=page.getByRole('button',{name:'Implement ready issues, acme/repo, Running',exact:true});
 const header=page.locator('.wf-editor .panel-header');
 const enable=header.getByRole('button',{name:'Enable',exact:true});
 const pause=header.getByRole('button',{name:'Pause',exact:true});
 await pausedCard.locator('svg.lucide-pause').waitFor();
 await enable.locator('svg.lucide-play').waitFor();
 assert.equal(await header.getByText(/^(Enabled|Disabled|Saved draft)$/).count(),0);
 await enable.click();
 await runningCard.locator('svg.lucide-play').waitFor();
 await pause.locator('svg.lucide-pause').waitFor();
 assert.equal(await header.getByText(/^(Enabled|Disabled|Saved draft)$/).count(),0);
 assert.equal(getDocument().workflows[0].enabled,true);
 await page.getByLabel('Workflow name').fill('Enabled issue workflow');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].name,'Enabled issue workflow');
 await page.getByRole('button',{name:'Pause',exact:true}).click();
 await page.getByRole('button',{name:'Enabled issue workflow, acme/repo, Paused',exact:true}).locator('svg.lucide-pause').waitFor();
 await page.getByRole('button',{name:'Review PRs',exact:true}).click();await waitForSaved(page);assert.equal(getDocument().workflows.length,2);
 await page.getByRole('button',{name:'Preview matches',exact:true}).click();await page.getByText('#12 Example issue',{exact:true}).waitFor();assert.equal(getDocument().workflows.every(w=>!w.enabled),true);
 await page.reload();await selectRepository(page, 'acme/repo');assert.equal(await page.locator('.wf-step').count(),2);await page.locator('.wf-step').first().click();await page.getByLabel('Review step',{exact:true}).check();await page.getByRole('slider',{name:'Review passes',exact:true}).fill('3');await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,4);
 await selectRepository(page, 'acme/other');await page.locator('.wf-step').first().waitFor({state:'detached'});assert.equal(getDocument().workflows.length,2);
 const editor=await page.locator('.wf-editor').boundingBox(),dock=await page.locator('.page-bottom-dock').boundingBox();assert.ok(editor.y+editor.height<=dock.y);});

test('GitHub Actions failure requires a specific workflow and previews run attempts', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await waitForSaved(page);
 await page.getByLabel('Trigger type').selectOption({label:'GitHub Actions failure'});
 await page.getByText('Enter a workflow filename or numeric workflow ID.',{exact:true}).waitFor();
 assert.equal(getDocument().workflows[0].trigger.event,'issue.labeled');
 await page.getByLabel('Workflow filename or ID').fill('deploy-prod.yml');
 await waitForSaved(page);
 assert.deepEqual(getDocument().workflows[0].trigger,{event:'action.failed',value:'deploy-prod.yml'});
 assert.equal(await page.locator('[data-id="filters"]').count(),0);
 await page.route('**/api/workflows/preview',async route=>{
  assert.deepEqual(route.request().postDataJSON().trigger,{event:'action.failed',value:'deploy-prod.yml'});
  await route.fulfill({json:[{
   item:{kind:'action',actionRunId:42000000001,actionRunAttempt:2,title:'Release'},
   matches:true,reason:'GitHub Actions run failed',
  }]});
 });
 await page.getByRole('button',{name:'Preview matches',exact:true}).click();
 await page.getByText('Run #42000000001, attempt 2 · Release',{exact:true}).waitFor();
});

for (const kind of ['review','repair']) {
 test('Switching a filtered '+kind+' workflow to Actions clears incompatible settings', async t => {
  const {page,getDocument}=await openEditor(t);
  await selectRepository(page,'acme/repo');
  await page.getByRole('button',{name:'Review PRs',exact:true}).click();
  await waitForSaved(page);
  const original=getDocument().workflows[0];
  original.task.kind=kind;
  original.filters={mode:'all',rules:[{field:'label',operator:'is',value:'ready'}]};
  await page.reload();
  await selectRepository(page,'acme/repo');
  await page.locator('.wf-step').first().click();
  await page.getByLabel('Trigger type').selectOption('action.failed');
  await page.getByLabel('Workflow filename or ID').fill('build.yml');
  await waitForSaved(page);
  const saved=getDocument().workflows[0];
  assert.equal(saved.trigger.event,'action.failed');
  assert.equal(saved.task.kind,'implement');
  assert.deepEqual(saved.filters,{mode:'all',rules:[]});
  assert.equal(await page.locator('[data-id="filters"]').count(),0);
 });
}

test('navigation saves valid edits and leaves incomplete changes at the last saved revision', async t => {
 const {page}=await openEditor(t,undefined,undefined,undefined,false);
 const getDocument=async()=>(await page.request.get(baseURL+'/api/workflows')).json();
 const suffix=crypto.randomUUID().slice(0,8);
 const navigationName='Saved on navigation '+suffix;
 const reloadName='Saved on reload '+suffix;
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await waitForSaved(page);
 const name=page.getByLabel('Workflow name');
 await name.fill(navigationName);
 await page.evaluate(()=>{location.hash='studio-builder'});
 await page.locator('#workflow-display').waitFor({state:'hidden'});
 await page.locator('#workflow-dock-link').click();
 await waitForSaved(page);
 const saved=await getDocument();
 const definition=saved.workflows.find(w=>w.name===navigationName);
 assert.ok(definition);

 await page.locator('[data-id="filters"]').click();
 await page.getByRole('button',{name:'Add filter',exact:true}).click();
 await page.locator('.wf-save-state').filter({hasText:/^Incomplete$/}).waitFor();
 await page.waitForTimeout(600);
 assert.equal((await getDocument()).revision,saved.revision);
 await page.reload();
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:new RegExp(navigationName)}).click();
 assert.deepEqual((await getDocument()).workflows.find(w=>w.id===definition.id).filters.rules,[]);
 await page.locator('[data-id="filters"]').click();
 assert.equal(await page.getByRole('button',{name:'Remove rule',exact:true}).count(),0);

 await name.fill(reloadName);
 await page.reload();
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:new RegExp(reloadName)}).waitFor();
 assert.equal((await getDocument()).workflows.find(w=>w.id===definition.id).name,reloadName);
});

test('autosave refreshes the document revision and retries after a concurrent save', async t => {
 const {page,getDocument,setDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await waitForSaved(page);
 const workflow = getDocument().workflows[0];
 const revision = getDocument().revision;
 setDocument({...getDocument(),revision:revision+1});
 await page.getByLabel('Workflow name').fill('Retried after concurrent save');
 await waitForSaved(page);
 assert.equal(getDocument().revision,revision+2);
 assert.equal(getDocument().workflows.find(w=>w.id===workflow.id).name,'Retried after concurrent save');
});

test('large pending autosaves survive leaving the page and restore on reload', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await waitForSaved(page);
 const workflow=getDocument().workflows[0];
 const pending={...workflow,name:'Restored large workflow',task:{...workflow.task,instructions:'x'.repeat(70_000)}};
 await page.evaluate(value=>sessionStorage.setItem('hubui.workflowPendingSave.v1',JSON.stringify(value)),{workflow:pending,base:workflow});
 await page.reload();
 await page.waitForFunction(()=>sessionStorage.getItem('hubui.workflowPendingSave.v1')===null);
 const restored=getDocument().workflows.find(w=>w.id===workflow.id);
 assert.equal(restored.name,pending.name);
 assert.equal(restored.task.instructions.length,70_000);
});

test('autosave keeps newer edits while an earlier revision is in flight', async t => {
 const {page,getDocument,onSave}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await waitForSaved(page);
 const revision=getDocument().revision;
 let finishSave;
 const saving=new Promise(resolve=>{finishSave=resolve});
 onSave(next=>next.workflows[0].name==='First edit' ? saving : undefined);
 const name=page.getByLabel('Workflow name');
 await name.fill('First edit');
 try {
  await page.locator('.wf-save-state').filter({hasText:/^Saving…$/}).waitFor();
  await name.fill('Latest edit');
 } finally {
  finishSave();
 }
 await waitForSaved(page);
 assert.equal(await name.inputValue(),'Latest edit');
 assert.equal(getDocument().revision,revision+2);
 assert.equal(getDocument().workflows[0].name,'Latest edit');
});
test('mobile editor fits the viewport in both normal and fullscreen layouts',async t=>{
 const {page}=await openEditor(t,{width:390,height:844});
 await selectRepository(page, 'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByLabel('Label',{exact:true}).fill('ready');
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 const maximize=page.getByRole('button',{name:'Maximize Workflow'});
 await maximize.click();
 const panel=page.locator('#workflow-display');
 assert.ok(await panel.evaluate(node=>node.scrollWidth<=node.clientWidth));
 await page.getByRole('button',{name:'Exit full screen Workflow'}).click();
 await maximize.waitFor();
 assert.equal(await page.getByLabel('Label',{exact:true}).inputValue(),'ready');
});

test('scheduled template supports library prompts, interval units, and inline fallback',async t=>{const {page,getDocument}=await openEditor(t);await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Scheduled task',exact:true}).click();assert.equal(await page.locator('.wf-node').count(),3);await page.getByRole('button',{name:'days',exact:true}).click();await page.locator('[data-id="task"]').click();await page.getByLabel('Prompt source',{exact:true}).selectOption('audit');await page.getByText('Preview library prompt',{exact:true}).click();await page.getByText('Audit the repository.',{exact:true}).waitFor();await page.getByLabel('Additional context (optional)').fill('Focus on security');await waitForSaved(page);assert.equal(getDocument().workflows[0].trigger.event,'schedule');assert.equal(getDocument().workflows[0].task.libraryTaskName,'audit');await page.getByLabel('Prompt source',{exact:true}).selectOption('');await page.getByLabel('Instructions',{exact:true}).fill('Run an inline repository task.');await waitForSaved(page);assert.equal(getDocument().workflows[0].task.libraryTaskName,undefined);});

test('retired automation templates create independent disabled workflows for one repository',async t=>{
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 await waitForSaved(page);
 await page.getByRole('button',{name:'Issue to merge',exact:true}).click();
 await waitForSaved(page);
 const workflows=getDocument().workflows;
 assert.equal(workflows.length,2);
 assert.ok(workflows.every(w=>w.repository==='acme/repo'&&!w.enabled&&!w.includeBacklog));
 assert.equal(workflows[0].trigger.event,'issue.assigned');
 assert.equal(workflows[0].trigger.value,'@me');
 assert.equal(workflows[0].reviewCycles,2);
 assert.equal(workflows[0].merge.enabled,false);
 assert.deepEqual(workflows[1].merge,{enabled:true,method:'squash',requireApproval:true,deleteBranch:true});
 assert.notEqual(workflows[0].id,workflows[1].id);
});

test('cached owner picker filters repositories and saves before switching', async t => {
 const {page,getDocument}=await openEditor(t);
 assert.equal(await page.getByPlaceholder('owner/repository').count(),0);
 assert.equal(await page.getByRole('button',{name:'Implement issues',exact:true}).isDisabled(),true);
 await page.getByRole('group',{name:'Organizations & people',exact:true}).getByRole('button').filter({hasText:'person'}).click();
 await page.getByRole('button',{name:'person/project',exact:true,pressed:true}).waitFor();
 assert.equal(await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button').count(),1);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByRole('group',{name:'Organizations & people',exact:true}).getByRole('button').filter({hasText:'person'}).click();
 await page.getByRole('button',{name:'person/project',exact:true,pressed:true}).waitFor();
 assert.equal(getDocument().workflows[0].repository,'acme/repo');
});

test('a single Workflow owner is selected while multiple repositories remain a choice', async t => {
 const {page}=await openEditor(t,undefined,{workflow_enabled:true,disabled_repositories:['person/project']});
 const picker=page.locator('.wf-repository-picker');
 const owners=picker.getByRole('group',{name:'Organizations & people',exact:true});
 const repositories=picker.getByRole('group',{name:'Repositories',exact:true});
 await owners.getByRole('button',{pressed:true}).waitFor();
 assert.deepEqual(await repositories.getByRole('button',{pressed:false}).allTextContents(),['acme/other','acme/repo']);
 for (const text of ['Organizations & people','Repositories']) {
  assert.equal(await picker.getByText(text,{exact:true}).count(),0);
 }
 assert.equal(await page.getByRole('button',{name:'Implement issues',exact:true}).isDisabled(),true);
});

test('schedule sliders select bounded counts and preserve minute and month intervals on reload', async t => {
 const {page,getDocument}=await openEditor(t,{width:390,height:844});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Scheduled task',exact:true}).click();
 const amount=unit=>page.getByRole('slider',{name:`Repeat every (${unit})`,exact:true});
 assert.equal(await page.getByText(/Without a first-run time/).count(),0);
 assert.equal(await page.getByLabel(/First run \(optional/).isVisible(),true);
 await page.getByRole('button',{name:'minutes',exact:true}).click();
 assert.equal(await amount('minutes').inputValue(),'5');
 await amount('minutes').press('ArrowLeft');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].trigger.intervalHours,5/60);
 await amount('minutes').fill('7');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].trigger.intervalHours,7/60);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await amount('minutes').inputValue(),'7');
 const slider=page.getByRole('slider',{name:'Repeat interval scale'});
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+8,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:12});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'months');
 await amount('months').fill('2');
 await waitForSaved(page);assert.equal(getDocument().workflows[0].trigger.intervalHours,1440);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await amount('months').inputValue(),'2');
 await slider.press('Home');await slider.press('ArrowRight');
 assert.equal(await slider.getAttribute('aria-valuetext'),'hours');
 for (const [unit,min,max,hours] of [['minutes',5,60,1],['hours',1,24,24],['months',1,12,8640],['days',1,7,168],['weeks',1,4,672]]) {
  await page.getByRole('button',{name:unit,exact:true}).click();
  assert.equal(await amount(unit).getAttribute('min'),String(min));
  assert.equal(await amount(unit).getAttribute('max'),String(max));
  await amount(unit).press('End');
  await waitForSaved(page);
  assert.equal(getDocument().workflows[0].trigger.intervalHours,hours);
 }
 const scale=await slider.boundingBox(),count=await amount('weeks').boundingBox();assert.ok(scale.y+scale.height<count.y);
 assert.equal(await page.locator('.wf-interval-units svg').count(),5);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 await page.screenshot({path:join(tmpdir(),'agent00-schedule-scale-mobile.png'),fullPage:true});
});

test('editing a scheduled workflow preserves its legacy count until the slider moves', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Scheduled task',exact:true}).click();
 await waitForSaved(page);
 const workflow=getDocument().workflows[0];
 workflow.trigger.intervalHours=10*24;
 await page.goto(`${baseURL}/#workflow/${workflow.id}`);
 await page.reload();
 await page.locator('[data-id="trigger"]').click();
 const amount=page.getByRole('slider',{name:'Repeat every (days)',exact:true});
 assert.equal(await amount.inputValue(),'7');
 assert.equal(await page.getByText('10 (saved)',{exact:true}).isVisible(),true);
 await page.getByLabel('Workflow name').fill('Renamed scheduled workflow');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].trigger.intervalHours,10*24);
 await amount.press('ArrowLeft');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].trigger.intervalHours,6*24);
 assert.equal(await page.getByText('10 (saved)',{exact:true}).count(),0);
});

test('in-app workflows keep defaults opt-in and compose review and merge steps', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 assert.equal(await page.locator('.wf-node').count(),3);
 assert.equal(await page.locator('[data-id="trigger"] .lucide-square-pen').count(),1);
 await page.getByLabel('Review step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),5);
 await page.getByRole('slider',{name:'Review passes',exact:true}).fill('2');
 await page.getByLabel('Merge step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),6);
 await waitForSaved(page);
 const w=getDocument().workflows[0];
 assert.equal(w.trigger.event,'app.prompt');assert.equal(w.enabled,false);assert.equal(w.reviewCycles,2);assert.equal(w.merge.enabled,true);
 await page.getByLabel('Review step',{exact:true}).uncheck();
 await page.getByLabel('Merge step',{exact:true}).uncheck();
 assert.equal(await page.locator('.wf-node').count(),3);
 await page.locator('[data-id="task"]').click();
 assert.equal(await page.getByLabel('Prompt source',{exact:true}).count(),0);
 await page.getByLabel('Additional instructions (optional)',{exact:true}).fill('Keep the submitted task scope.');
 await waitForSaved(page);
 await page.getByRole('button',{name:'JSON submission',exact:true}).click();
 assert.equal(await page.locator('[data-id="trigger"] .lucide-file-json').count(),1);
});

test('review cycles loop through an independently configurable review agent', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 const reviewStep=page.getByLabel('Review step',{exact:true});
 const edge=page.getByTestId('rf__edge-review-reviewAgent');
 const label=page.locator('.wf-review-cycle-label');
 assert.equal(await edge.count(),0);
 await reviewStep.check();
 await label.getByText('1',{exact:true}).waitFor();
 assert.equal(await label.locator('svg.lucide-refresh-ccw-dot').count(),1);
 assert.equal(await edge.getAttribute('aria-label'),'Review cycles and review agent, up to 1 cycle');
 const paths=edge.locator('.react-flow__edge-path');
 assert.equal(await paths.count(),2);
 const marker=await paths.first().getAttribute('marker-end');
 assert.match(marker,/arrowclosed/);
 assert.equal(await paths.last().getAttribute('marker-end'),marker);
 assert.match(await paths.last().getAttribute('d'),/ Q /);

 await page.getByRole('button',{name:'4 passes',exact:true}).click();
 await label.getByText('4',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Arrange workflow horizontally',exact:true}).click();
 await page.waitForFunction(()=>{
  const edge=document.querySelector('[data-testid="rf__edge-review-reviewAgent"]');
  const path=edge?.querySelector('.react-flow__edge-path');
  const background=document.querySelector('.wf-review-cycle-label');
  if (!path || !background) return false;
  const length=path.getTotalLength();
  const start=path.getPointAtLength(0);
  const end=path.getPointAtLength(length);
  const middle=path.getPointAtLength(length/2).matrixTransform(path.getScreenCTM());
  const box=background.getBoundingClientRect();
  return Math.abs(start.y-end.y)>Math.abs(start.x-end.x)
    && Math.abs(box.x+box.width/2-middle.x)<2 && Math.abs(box.y+box.height/2-middle.y)<2;
 });
 const agent=page.locator('[data-id="reviewAgent"]');
 await agent.click();
 const prompt=page.getByLabel('Review prompt (optional)',{exact:true});
 const complexity=page.getByLabel('Review complexity',{exact:true});
 await prompt.fill('Check error handling and recovery.');
 await complexity.focus();
 await complexity.press('End');
 assert.equal(await page.getByLabel('Base branch (optional)',{exact:true}).count(),0);
 assert.equal(await page.getByLabel('Target directory (optional)',{exact:true}).count(),0);
 await waitForSaved(page);
 assert.deepEqual(getDocument().workflows[0].reviewAgent,{instructions:'Check error handling and recovery.',complexity:'advanced'});
 await page.reload();
 await selectRepository(page,'acme/repo');
 await page.locator('.wf-step').first().click();
 await agent.click();
 assert.equal(await prompt.inputValue(),'Check error handling and recovery.');
 assert.equal(await complexity.getAttribute('aria-valuetext'),'Advanced');
 await page.locator('.react-flow__controls-fitview').click();
 await page.waitForFunction(()=>{
  const canvas=document.querySelector('.react-flow').getBoundingClientRect();
  return [...document.querySelectorAll('.react-flow__node')].every(node=>{
   const box=node.getBoundingClientRect();
   return box.left>=canvas.left&&box.right<=canvas.right&&box.top>=canvas.top&&box.bottom<=canvas.bottom;
  });
 });
 await page.screenshot({path:join(tmpdir(),'agent00-workflow-review-agent.png'),fullPage:true});
 await reviewStep.uncheck();
 await edge.waitFor({state:'detached'});
 await label.waitFor({state:'detached'});
 await agent.waitFor({state:'detached'});
});

test('review pass slider supports presets, keyboard, dragging and existing saved values', async t => {
 const {page,getDocument}=await openEditor(t,{width:390,height:844});
 await selectRepository(page,'acme/repo');await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 await page.getByLabel('Review step',{exact:true}).check();
 const slider=page.getByRole('slider',{name:'Review passes',exact:true});
 const presets=page.getByRole('group',{name:'Review pass presets'});
 assert.deepEqual(await presets.getByRole('button').allTextContents(),['','','','','','']);
 for(const icon of ['signal-zero','signal-low','signal-medium','signal-high','signal']) {
  assert.equal(await presets.locator(`svg.lucide-${icon}`).count(),icon==='signal'?2:1);
 }
 assert.equal(await page.getByText(/A fresh review agent checks the changes/).count(),0);
 assert.equal(await page.getByRole('spinbutton').count(),0);
 for(const count of [0,1,2,4,8,10]) {
  await page.getByRole('button',{name:`${count} ${count===1?'pass':'passes'}`,exact:true}).click();
  assert.equal(await slider.inputValue(),String([0,1,2,4,8,10].indexOf(count)));
 }
 await slider.focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowRight');
 assert.equal(await slider.getAttribute('aria-valuetext'),'1 pass');
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+box.width/4,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:8});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'10 passes');
 await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,10);
 getDocument().workflows[0].reviewCycles=9;
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="review"]').click();
 assert.equal(await slider.getAttribute('aria-valuetext'),'9 passes, saved value; choose a preset to change');
 assert.equal(await presets.getByRole('button',{pressed:true}).count(),0);
 await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,9);
 await page.getByRole('button',{name:'4 passes',exact:true}).click();await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,4);
 await page.screenshot({path:join(tmpdir(),'agent00-review-pass-slider.png'),fullPage:true});
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
});

test('dragging a workflow node keeps nodes and edges visible', async t => {
 const {page,getDocument}=await openEditor(t,{width:1600,height:1050});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 await waitForSaved(page);

 await page.getByRole('button',{name:'Maximize Workflow'}).click();
 await page.locator('.react-flow__controls-fitview').click();

 const node=page.locator('.react-flow__node[data-id="task"]');
 const source=node.locator('.react-flow__handle.source');
 await page.waitForFunction(()=>document.querySelectorAll('.react-flow__edge-path').length===6);
 await node.hover();
 const initialTransform=await node.evaluate(el=>el.style.transform);
 const start=await node.boundingBox();
 const x=start.x+start.width/2, y=start.y+start.height/2;
 await page.mouse.move(x,y);
 await page.mouse.down();
 await page.evaluate(()=>{
  window.dragFrames=[];
  function sample() {
   window.dragFrames.push({
    edges:document.querySelectorAll('.react-flow__edge-path').length,
    hidden:!!document.querySelector('.react-flow__node[style*="visibility: hidden"]'),
   });
   window.dragFrame=requestAnimationFrame(sample);
  }
  window.dragFrame=requestAnimationFrame(sample);
 });
 await page.mouse.move(x+240,y+75,{steps:24});
 const frames=await page.evaluate(()=>{
  cancelAnimationFrame(window.dragFrame);
  return window.dragFrames;
 });
 assert.ok(frames.length>1);
 assert.ok(frames.every(frame=>frame.edges===6&&!frame.hidden), 'nodes and edges stay visible while dragging');
 assert.equal(await source.getAttribute('data-handlepos'),'bottom');
 const dragged=await node.boundingBox();
 assert.notEqual(await node.evaluate(el=>el.style.transform),initialTransform);
 assert.ok(dragged.x>start.x+200);
 const saveDraft=page.getByRole('button',{name:'Enable',exact:true});
 await node.focus();
 await page.keyboard.press('ArrowRight');
 assert.equal(await saveDraft.isDisabled(),true,'keyboard position changes do not clear the pointer drag guard');
 const draggedTransform=await node.evaluate(el=>el.style.transform);
 const name=page.getByLabel('Workflow name');
 await name.focus();
 await page.keyboard.press('End');
 await page.keyboard.type(' updated');
 assert.match(await name.inputValue(),/ updated$/);
 assert.equal(await saveDraft.isDisabled(),true);
 assert.equal(await node.evaluate(el=>el.style.transform),draggedTransform,'settings edits keep the live drag position');
 await page.mouse.up();
 await node.locator('.react-flow__handle.source[data-handlepos="left"]').waitFor({state:'attached'});
 const dropped=await node.boundingBox();
 assert.ok(Math.abs(dropped.x-dragged.x)<2,'node stays where it was dropped');
 await waitForSaved(page);
 assert.ok(getDocument().workflows[0].positions.task.x>80);
 assert.match(getDocument().workflows[0].name,/ updated$/);
});

test('switching workflows by keyboard during a drag leaves the new workflow untouched', async t => {
 const {page,getDocument}=await openEditor(t,{width:1600,height:1050});
 await selectRepository(page,'acme/repo');
 const name=page.getByLabel('Workflow name');
 for (const [template, title] of [['Assigned issues','First workflow'],['Prompt submission','Second workflow']]) {
  await page.getByRole('button',{name:template,exact:true}).click();
  await name.fill(title);
  await waitForSaved(page);

 }
 const node=page.locator('.react-flow__node[data-id="task"]');
 const savedSecond=getDocument().workflows.find(w=>w.name==='Second workflow');
 const position=el=>el.style.transform;
 const secondPosition=await node.evaluate(position);
 await page.getByRole('button',{name:/First workflow/}).click();
 await page.locator('.react-flow__controls-fitview').click();
  const firstPosition=await node.evaluate(position);

 const {x,y}=await visibleNodeCenter(page,node);
 await page.mouse.move(x,y);
 await page.mouse.down();
 await page.mouse.move(x+100,y,{steps:8});
 assert.notEqual(await node.evaluate(position),firstPosition);
 const second=page.getByRole('button',{name:/Second workflow/});
 await second.focus();
 await page.keyboard.press('Enter');
 assert.equal(await name.inputValue(),'First workflow','switching is blocked while the pointer drag is active');
 assert.match(await page.locator('.wf-notice').textContent(),/Finish moving the workflow/);
 await page.mouse.move(x+120,y,{steps:4});
 await page.mouse.up();
 assert.equal(await name.inputValue(),'First workflow');
 assert.notEqual(await node.evaluate(position),firstPosition);
 await second.click();
 assert.equal(await name.inputValue(),'Second workflow');
 assert.deepEqual(getDocument().workflows.find(w=>w.id===savedSecond.id),savedSecond);
});

test('workflow save and activation controls cannot interrupt a node drag', async t => {
 const {page,getDocument}=await openEditor(t,{width:1600,height:1050});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();

 const enable=page.getByRole('button',{name:'Enable',exact:true});
 const pause=page.getByRole('button',{name:'Pause',exact:true});
 await waitForSaved(page);

 const node=page.locator('.react-flow__node[data-id="task"]');
 for (const toggle of [enable,pause]) {

  await page.locator('.react-flow__controls-fitview').click();

  const {x,y}=await visibleNodeCenter(page,node);
  await page.mouse.move(x,y);
  await page.mouse.down();
  await page.mouse.move(x+70,y,{steps:8});
  const revision=getDocument().revision;
  const previousX=getDocument().workflows[0].positions?.task?.x ?? 80;
  for (const button of [toggle]) {
   assert.equal(await button.isDisabled(),true);
   await button.focus();
   await page.keyboard.press('Enter');
  }
  await page.mouse.up();

  assert.equal(getDocument().revision,revision);
  await page.getByLabel("Workflow name").fill("Moved "+previousX);
  await waitForSaved(page);
  assert.ok(getDocument().workflows[0].positions.task.x>previousX);
  if (toggle===enable) {
   await enable.click();
   await pause.waitFor();
  }
 }
});

test('a pending workflow save prevents starting a drag', async t => {
 const {page}=await openEditor(t,{width:1600,height:1050});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 let finishSave;
 const pendingSave=new Promise(resolve=>{finishSave=resolve});
 await page.route('**/api/workflows',async route=>{
  if (route.request().method()==='PUT') await pendingSave;
  await route.fallback();
 });
 try {
  const node=page.locator('.react-flow__node[data-id="task"]');
  await page.locator('.react-flow__controls-fitview').click();
  await node.hover();
  const position=await node.evaluate(el=>el.style.transform);

  await page.getByLabel("Workflow name").fill("Pending save");
  await page.getByText("Saving…",{exact:true}).waitFor();
  const box=await node.boundingBox();
  const x=box.x+box.width/2, y=box.y+box.height/2;
  await page.mouse.move(x,y);
  await page.mouse.down();
  await page.mouse.move(x+70,y,{steps:8});
  await page.mouse.up();
  assert.equal(await node.evaluate(el=>el.style.transform),position);
 } finally {
  finishSave();
 }
 await waitForSaved(page);
});

test('reselecting the active workflow fits its graph after panning', async t => {
 const {page}=await openEditor(t,{width:1600,height:1050});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 await waitForSaved(page);
 const workflow=page.locator('.wf-step.wf-active');
 const canvas=page.locator('.react-flow');
 const node=page.locator('.react-flow__node[data-id="task"]');
 await node.hover();
 const bounds=await canvas.boundingBox();
 const y=bounds.y+bounds.height/2;
 await page.mouse.move(bounds.x+bounds.width-20,y);
 await page.mouse.down();
 await page.mouse.move(bounds.x+20,y,{steps:12});
 await page.mouse.up();
 const panned=await node.boundingBox();
 assert.ok(panned.x+panned.width<bounds.x,'panning moves the task off-screen');

 await workflow.click();
 await page.waitForFunction(()=>{
  const canvas=document.querySelector('.react-flow').getBoundingClientRect();
  const nodes=[...document.querySelectorAll('.react-flow__node')];
  return nodes.length>0 && nodes.every(node=>{
   const box=node.getBoundingClientRect();
   return box.left>=canvas.left && box.right<=canvas.right && box.top>=canvas.top && box.bottom<=canvas.bottom;
  });
 },null,{timeout:3000});
});

test('layout changes stay local until a workflow settings edit', async t => {
 const {page,getDocument}=await openEditor(t);
 const vertical=page.getByRole('button',{name:'Arrange workflow vertically',exact:true});
 const horizontal=page.getByRole('button',{name:'Arrange workflow horizontally',exact:true});
 assert.equal(await vertical.isDisabled(),true);assert.equal(await horizontal.isDisabled(),true);
 await selectRepository(page,'acme/repo');await page.getByRole('button',{name:'Issue to merge',exact:true}).click();
 assert.equal(await horizontal.locator('svg').evaluate(svg=>getComputedStyle(svg).fill),'none');
 await waitForSaved(page);
 const initialRevision=getDocument().revision;
 const node=page.locator('[data-id="trigger"]');
 const box=await node.boundingBox();
 await page.mouse.move(box.x+box.width/2,box.y+box.height/2);
 await page.mouse.down();
 await page.mouse.move(box.x+box.width/2+30,box.y+box.height/2+20,{steps:4});
 await page.mouse.up();
 assert.notDeepEqual(await node.boundingBox(),box);
 await horizontal.click();
 await page.waitForTimeout(600);
 assert.equal(getDocument().revision,initialRevision);
 await page.getByLabel('Workflow name').fill('Horizontal workflow');
 await waitForSaved(page);
 const horizontalPositions=structuredClone(getDocument().workflows[0].positions);
 const steps=['repository','trigger','filters','task','review','merge'];
 const values=steps.map(step=>horizontalPositions[step]);
 assert.equal(Object.keys(horizontalPositions).length,7);assert.equal(new Set(values.map(p=>p.y)).size,1);
 assert.equal(horizontalPositions.reviewAgent.x,horizontalPositions.review.x);
 assert.ok(horizontalPositions.reviewAgent.y>horizontalPositions.review.y+100);
 for(let i=1;i<values.length;i++)assert.ok(values[i].x>values[i-1].x+230);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-right').count(),1);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.deepEqual(getDocument().workflows[0].positions,horizontalPositions);
 await vertical.click();
 await page.getByLabel('Workflow name').fill('Vertical workflow');
 await waitForSaved(page);
 const positions=getDocument().workflows[0].positions;
 const verticalPositions=steps.map(step=>positions[step]);
 assert.equal(positions.reviewAgent.y,positions.review.y);
 assert.ok(positions.reviewAgent.x>positions.review.x+230);
 assert.equal(new Set(verticalPositions.map(p=>p.x)).size,1);
 for(let i=1;i<verticalPositions.length;i++)assert.ok(verticalPositions[i].y>verticalPositions[i-1].y+100);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-top').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-bottom').count(),1);
 assert.equal(getDocument().workflows[0].merge.enabled,true);
 await page.screenshot({path:join(tmpdir(),'agent00-workflow-layout.png'),fullPage:true});
});

test('Workflow shows sanitized discovery failures, preserves stale choices, and retries', async t => {
 const limited={ok:false,error:'GitHub API rate limited; retry at 2026-12-01T12:30:00Z (private upstream details)',repos:[]};
 let discovery=limited;
 const {page}=await openEditor(t,undefined,undefined,undefined,true,url=>url.searchParams.has('include_disabled') ? defaultRepos : discovery);
 const picker=page.locator('.wf-repository-picker');
 const alert=picker.getByRole('alert');
 const empty=picker.getByText('No repositories available. Check repository access in Settings.');
 const retry=picker.getByRole('button',{name:'Retry repositories',exact:true});
 await alert.waitFor();
 assert.match(await alert.textContent(),/Retry after 2026-12-01 12:30:00 UTC/);
 assert.doesNotMatch(await alert.textContent(),/private upstream details/);
 assert.equal(await empty.count(),0);

 // A Settings visibility update supplies a previously discovered list while the failure persists.
 await page.waitForFunction(()=>window.RepositoryVisibility.repositories()?.length===3);
 await page.evaluate(()=>window.dispatchEvent(new Event('repository-visibility-changed')));
 const knownOwner=picker.getByRole('button',{name:/acme/});
 await knownOwner.waitFor();
 assert.match(await alert.textContent(),/list may be stale/);
 assert.equal(await retry.isEnabled(),true);

 discovery={ok:true,repos:[{full_name:'example/recovered'}]};
 await retry.click();
 const recovered=picker.getByRole('button',{name:'example/recovered',exact:true,pressed:true});
 await recovered.waitFor();
 assert.equal(await alert.count(),0);
 assert.equal(await knownOwner.count(),0);

 discovery={ok:false,error:'gh: API rate limit exceeded for user (private upstream details)',repos:[]};
 await page.evaluate(()=>{
  window.RepositoryVisibility.repositories=()=>null;
  window.dispatchEvent(new Event('repository-visibility-changed'));
 });
 await alert.waitFor();
 assert.match(await alert.textContent(),/Retry after the rate limit resets.*list may be stale/);
 assert.doesNotMatch(await alert.textContent(),/private upstream details/);
 assert.equal(await recovered.isVisible(),true);
 assert.equal(await empty.count(),0);
});

test('Workflow shows the empty state after successful empty discovery', async t => {
 const {page}=await openEditor(t,undefined,undefined,undefined,true,()=>({ok:true,repos:[]}));
 const picker=page.locator('.wf-repository-picker');
 await picker.getByText('No repositories available. Check repository access in Settings.').waitFor();
 assert.equal(await picker.getByRole('alert').count(),0);
 assert.equal(await picker.getByRole('button',{name:'Retry repositories'}).count(),0);
});

test('repository visibility is shared across Workflow, Studio, Chat, and Issues after Settings changes', async t => {
 const {page}=await openEditor(t,undefined,{workflow_enabled:true,chat_enabled:true,github_issues_enabled:true,disabled_repositories:['https://github.com/acme/other','person/project']});
 const picker=page.locator('.wf-repository-picker');
 const owners=picker.getByRole('group',{name:'Organizations & people',exact:true});
 const repositories=picker.getByRole('group',{name:'Repositories',exact:true});
 await owners.getByRole('button',{name:/acme/,pressed:true}).waitFor();
 await repositories.getByRole('button',{name:'acme/repo',exact:true,pressed:true}).waitFor();
 assert.deepEqual(await repositories.getByRole('button').allTextContents(),['acme/repo']);
 await page.locator('#settings-open').click();
 await page.getByRole('tab',{name:'Repositories',exact:true}).click();
 await page.locator('#review-settings-owners [data-repo-owner="acme"]').click();
 await page.locator('#review-settings-close').click();
 await page.locator('#settings-gate').waitFor({state:'hidden'});
 await owners.waitFor({state:'detached'});
 await picker.getByText('No repositories available. Check repository access in Settings.').waitFor();
 assert.equal(await picker.getByRole('alert').count(),0);
 assert.equal(await page.locator('#builder-repo-select option[value*="acme/"]').count(),0);
 assert.equal(await page.locator('#library-repo-select option[value*="acme/"]').count(),0);
 await page.waitForFunction(()=>document.querySelectorAll('#github-issues-repo option[value="acme/repo"]').length===0);
 assert.equal(await page.locator('#chat-repo-grid .chat-repo-card').count(),0);
 await page.locator('#settings-open').click();
 await page.getByRole('tab',{name:'Repositories',exact:true}).click();
 await page.locator('#review-settings-owners [data-repo-owner="person"]').click();
 await page.locator('#review-settings-close').click();
 await page.locator('#settings-gate').waitFor({state:'hidden'});
 await owners.getByRole('button',{name:/person/,pressed:true}).waitFor();
 await repositories.getByRole('button',{name:'person/project',exact:true,pressed:true}).waitFor();
 assert.deepEqual(await repositories.getByRole('button').allTextContents(),['person/project']);
 assert.equal(await page.locator('#builder-repo-select option[value*="person/project"]').count(),1);
 await page.waitForFunction(()=>document.querySelectorAll('#github-issues-repo option[value="person/project"]').length===1);
});

test('Workflow draft controls are icon buttons in the header and preview stays available', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 const header=page.locator('#workflow-display .panel-header');
 assert.equal(await header.getByRole('button',{name:'Save draft',exact:true}).count(),0);
 for (const name of ['Delete draft','Enable']) {
  const button=header.getByRole('button',{name,exact:true});
  assert.equal(await button.getAttribute('title'),name);
  assert.equal(await button.textContent(),'');
  assert.equal(await button.locator('svg').count(),1);
 }
 assert.equal(await page.getByText('Activation history',{exact:true}).count(),0);
 await waitForSaved(page);
 await header.getByRole('button',{name:'Preview matches',exact:true}).click();
 await page.getByText('Matching preview (1 matches)',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Close preview',exact:true}).click();
 await header.getByRole('button',{name:'Delete draft',exact:true}).click();
 await page.getByText('Workflow deleted.',{exact:true}).waitFor();
 assert.equal(getDocument().workflows.length,0);
});

test('Workflow templates can be collapsed and reopened from the sidebar heading', async t => {
 const {page}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 const template=page.getByRole('button',{name:'Implement issues',exact:true});
 const hide=page.getByRole('button',{name:'Hide workflow templates'});
 const show=page.getByRole('button',{name:'Show workflow templates'});
 await template.waitFor();
 assert.equal(await hide.getAttribute('aria-expanded'),'true');
 await hide.click();
 await template.waitFor({state:'hidden'});
 assert.equal(await show.getAttribute('aria-expanded'),'false');
 assert.equal(await page.getByText('WORKFLOW TEMPLATES',{exact:true}).isVisible(),true);
 await show.click();
 await template.click();
 assert.equal(await page.getByLabel('Workflow name').inputValue(),'Implement issues');
});

test('Workflow header actions stay on one row at narrow desktop widths', async t => {
 const {page}=await openEditor(t,{width:720,height:900});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Scheduled task',exact:true}).click();
 const header=page.locator('#workflow-display .panel-header');
 const actions=header.getByRole('button');
 for (const width of [720,834]) {
  await page.setViewportSize({width,height:900});
  const panel=await header.boundingBox();
  const boxes=await Promise.all((await actions.all()).map(action=>action.boundingBox()));
  assert.equal(boxes.length,5);
  assert.ok(boxes.every(box=>Math.abs(box.y+box.height/2-(boxes[0].y+boxes[0].height/2))<1));
  assert.ok(boxes.every(box=>box.x>=panel.x && box.x+box.width<=panel.x+panel.width));
 }
});

test('Workflow uses the terminal fullscreen shell and preserves the draft on exit', async t => {
 const {page}=await openEditor(t,{width:1600,height:900});
 const panel=page.locator('#workflow-display');
 const maximize=page.getByRole('button',{name:'Maximize Workflow'});
 const restore=page.locator('#task-fullscreen-close');
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 const workflowName=page.getByRole('textbox',{name:'Workflow name'});
 await workflowName.fill('Unsaved workflow');
 const defaultSize=await panel.boundingBox();

 await maximize.click();
 const shell=page.locator('#task-fullscreen');
 assert.equal(await shell.getAttribute('data-mode'),'workflow');
 assert.equal(await panel.locator('xpath=..').getAttribute('id'),'task-fullscreen-body');
 assert.equal(await restore.getAttribute('aria-label'),'Exit full screen Workflow');
 assert.equal(await restore.evaluate(button=>button===document.activeElement),true);
 assert.equal(await page.locator('#task-fullscreen-output-panel').isVisible(),false);
 assert.equal(await page.locator('.task-fullscreen-task-panel').isVisible(),false);
 const expanded=await panel.boundingBox();
 assert.equal(expanded.x,20);
 assert.equal(expanded.y,20);
 assert.equal(expanded.width,1560);
 assert.ok(expanded.height>defaultSize.height);
 await restore.click();
 assert.deepEqual(await panel.boundingBox(),defaultSize);
 assert.equal(await maximize.evaluate(button=>button===document.activeElement),true);

 await maximize.click();
 await page.keyboard.press('Escape');
 await maximize.waitFor();
 assert.deepEqual(await panel.boundingBox(),defaultSize);

 await maximize.click();
 await page.evaluate(()=>{location.hash='builder'});
 await panel.waitFor({state:'hidden'});
 await page.locator('#workflow-dock-link').click();
 await maximize.waitFor();
 assert.deepEqual(await panel.boundingBox(),defaultSize);
 assert.equal(await workflowName.inputValue(),'Unsaved workflow');
});

test('Workflow uses the same Studio panel and titlebar styles', async t => {
 const {page}=await openEditor(t);
 assert.equal(await page.locator('#workflow-display .panel-header').getByText('Version 2', {exact:true}).count(), 0);
 // Compare settled theme styles rather than sampling the transition mid-frame.
 await page.addStyleTag({content:'.studio-panel { transition: none !important; }'});
 const styles=async selector=>page.locator(selector).evaluate(panel=>{
  const box=panel.getBoundingClientRect(), style=getComputedStyle(panel);
  const header=getComputedStyle(panel.querySelector('.panel-header'));
  const title=getComputedStyle(panel.querySelector('.panel-section-title'));
  return {x:box.x,y:box.y,width:box.width,height:box.height,radius:style.borderRadius,border:style.border,background:style.background,shadow:style.boxShadow,headerPadding:header.padding,headerBackground:header.background,headerBorder:header.borderBottom,titleFont:title.font,titleSpacing:title.letterSpacing};
 });
 for (const theme of ['light','dark','night','pink']) {
  await page.evaluate(theme=>{document.documentElement.classList.remove('light','dark','night','pink');document.documentElement.classList.add(theme);document.documentElement.dataset.theme=theme},theme);
  await page.evaluate(()=>{location.hash='workflow'});
  await page.locator('#workflow-display').waitFor({state:'visible'});
  const workflow=await styles('#workflow-display');
  await page.evaluate(()=>{location.hash='studio-builder'});
  await page.locator('.prompt-wrap').waitFor({state:'visible'});
  const studio=await styles('.prompt-wrap');
  assert.deepEqual(workflow,studio,`${theme}: shared Studio chrome and available space`);
 }
});

test('task complexity can override the application default and return to inheritance', async t => {
  const { page, getDocument } = await openEditor(t, undefined, {
    workflow_enabled: true, default_complexity: 'easy',
  });
  await selectRepository(page, 'acme/repo');
  await page.getByRole('button', { name: 'Scheduled task', exact: true }).click();
  await page.locator('[data-id="task"]').click();
  const complexity = page.getByRole('slider', { name: 'Complexity', exact: true });
  const inherited = (name) => page.getByRole('button', { name: `Use settings default (${name})`, exact: true });
  await inherited('Easy').locator('svg.lucide-rabbit').waitFor();
  assert.equal(await complexity.getAttribute('aria-valuetext'), 'Use settings default (Easy)');
  await complexity.press('End');
  await waitForSaved(page);
  assert.equal(getDocument().workflows[0].task.complexity, 'advanced');
  await inherited('Easy').click();
  await waitForSaved(page);
  assert.equal(getDocument().workflows[0].task.complexity, undefined);

  await page.locator('#settings-open').click();
  await page.locator('#settings-tab-automation').click();
  await page.getByRole('group', { name: 'Default complexity presets' }).getByRole('button', { name: 'Standard' }).click();
  await page.locator('#review-settings-close').click();
  await page.locator('#settings-gate').waitFor({ state: 'hidden' });
  await inherited('Standard').locator('svg.lucide-brain').waitFor();
  assert.equal(await complexity.getAttribute('aria-valuetext'), 'Use settings default (Standard)');
  assert.equal(getDocument().workflows[0].task.complexity, undefined);
});

test('workflow saves and clears repository PR routing options', async t => {
 const {page,getDocument}=await openEditor(t);
 let loads=0;
 await page.route('**/api/workflows/pr-options?repo=*', route => {
  assert.equal(new URL(route.request().url()).searchParams.get('repo'), 'acme/repo');
  if (++loads === 1) return route.fulfill({status:502,json:{error:'GitHub unavailable'}});
  return route.fulfill({json:{assignees:['maintainer','teammate'],labels:['tracking','priority, high']}});
 });
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.locator('[data-id="task"]').click();
 await page.getByRole('button',{name:'Retry loading PR options'}).click();
 await page.getByLabel('PR assignee (optional)',{exact:true}).selectOption('maintainer');
 await page.getByLabel('Additional PR labels',{exact:true}).selectOption(['tracking','priority, high']);
 await page.getByLabel('Create draft pull request').check();
 await waitForSaved(page);
 let saved=getDocument().workflows[0];
 assert.equal(saved.task.prAssignee,'maintainer');
 assert.deepEqual(saved.task.prLabels,['tracking','priority, high']);
 assert.equal(saved.task.draft,true);
 await page.reload();
 await selectRepository(page,'acme/repo');
 await page.locator('.wf-step').first().click();
 await page.locator('[data-id="task"]').click();
 const assignee=page.getByLabel('PR assignee (optional)',{exact:true});
 const labels=page.getByLabel('Additional PR labels',{exact:true});
 assert.equal(await assignee.inputValue(),'maintainer');
 assert.deepEqual(await labels.evaluate(el=>Array.from(el.selectedOptions,option=>option.value)),['tracking','priority, high']);
 await assignee.selectOption('');
 await labels.selectOption([]);
 await waitForSaved(page);
 saved=getDocument().workflows[0];
 assert.equal(saved.task.prAssignee,undefined);
 assert.equal(saved.task.prLabels,undefined);
});

test('workflow links open the saved definition directly', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByLabel('Workflow name').fill('Linked issue workflow');
 await waitForSaved(page);
 const saved = getDocument().workflows[0];
 await page.goto(`${baseURL}/#workflow/${encodeURIComponent(saved.id)}`);
 await page.reload();
 await page.getByLabel('Workflow name').waitFor();
 assert.equal(await page.getByLabel('Workflow name').inputValue(), 'Linked issue workflow');
});
