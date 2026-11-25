const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {tmpdir}=require('node:os');
const {join}=require('node:path');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
async function openEditor(t,viewport={width:1280,height:900},initialSettings={workflow_enabled:true},getAgentAuth=()=>({required:false,ready:true,state:'ready'}),mockStorage=true){
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
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{repos:[{full_name:'acme/repo'},{full_name:'acme/other'},{full_name:'person/project',owner_type:'User'}]}});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:await getAgentAuth()}});
 if(url.pathname==='/api/review-settings'){if(route.request().method()==='POST')settings=route.request().postDataJSON();return route.fulfill({json:{ok:true,settings}})};
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#workflow');await page.locator('#workflow-title').waitFor();return {page,getDocument:()=>doc,setDocument:value=>{doc=value},onSave:handler=>{beforeSave=handler}};
}
async function waitForSaved(page) {
 await page.locator('.wf-save-state:empty').waitFor({state:'attached'});
}
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

test('schedule scale drags between units and preserves minute and month intervals on reload', async t => {
 const {page,getDocument}=await openEditor(t,{width:390,height:844});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Scheduled task',exact:true}).click();
 await page.getByRole('button',{name:'minutes',exact:true}).click();
 await page.getByLabel('Repeat every (minutes)',{exact:true}).fill('7');
 await waitForSaved(page);
 assert.equal(getDocument().workflows[0].trigger.intervalHours,7/60);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (minutes)',{exact:true}).inputValue(),'7');
 const slider=page.getByRole('slider',{name:'Repeat interval scale'});
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+8,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:12});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'months');
 await page.getByLabel('Repeat every (months)',{exact:true}).fill('2');
 await waitForSaved(page);assert.equal(getDocument().workflows[0].trigger.intervalHours,1440);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (months)',{exact:true}).inputValue(),'2');
 await page.getByRole('slider',{name:'Repeat interval scale'}).focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowRight');
 assert.equal(await page.getByRole('slider').getAttribute('aria-valuetext'),'hours');
 await page.getByRole('button',{name:'weeks',exact:true}).click();await waitForSaved(page);assert.equal(getDocument().workflows[0].trigger.intervalHours,336);
 const scale=await page.getByRole('slider').boundingBox(),amount=await page.getByLabel('Repeat every (weeks)',{exact:true}).boundingBox();assert.ok(scale.y+scale.height<amount.y);
 assert.equal(await page.locator('.wf-interval-units svg').count(),5);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 await page.screenshot({path:join(tmpdir(),'agent00-schedule-scale-mobile.png'),fullPage:true});
});

test('in-app workflows keep defaults opt-in and compose review and merge steps', async t => {
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 assert.equal(await page.locator('.wf-node').count(),3);
 assert.equal(await page.locator('[data-id="trigger"] .lucide-square-pen').count(),1);
 await page.getByLabel('Review step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),4);
 await page.getByRole('slider',{name:'Review passes',exact:true}).fill('2');
 await page.getByLabel('Merge step',{exact:true}).check();
 assert.equal(await page.locator('.wf-node').count(),5);
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

test('the agent and review are connected by a bidirectional line with a live cycle count', async t => {
 const {page}=await openEditor(t);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 const reviewStep=page.getByLabel('Review step',{exact:true});
 const edge=page.getByTestId('rf__edge-task-review');
 const label=page.locator('.wf-review-cycle-label');
 assert.equal(await edge.count(),0);
 await reviewStep.check();
 await label.getByText('1',{exact:true}).waitFor();
 assert.equal(await label.locator('svg.lucide-refresh-ccw-dot').count(),1);
 assert.equal(await edge.getAttribute('aria-label'),'Agent task and review, up to 1 cycle');
 const path=edge.locator('.react-flow__edge-path');
 const marker=await path.getAttribute('marker-start');
 assert.match(marker,/arrowclosed/);
 assert.equal(await path.getAttribute('marker-end'),marker);
 assert.equal(await page.locator('.react-flow__arrowhead').getAttribute('orient'),'auto-start-reverse');

 await page.getByRole('button',{name:'4 passes',exact:true}).click();
 await label.getByText('4',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Arrange workflow horizontally',exact:true}).click();
 await page.waitForFunction(()=>{
  const edge=document.querySelector('[data-testid="rf__edge-task-review"]');
  const path=edge?.querySelector('.react-flow__edge-path');
  const background=document.querySelector('.wf-review-cycle-label');
  if (!path || !background) return false;
  const length=path.getTotalLength();
  const start=path.getPointAtLength(0);
  const end=path.getPointAtLength(length);
  const middle=path.getPointAtLength(length/2).matrixTransform(path.getScreenCTM());
  const box=background.getBoundingClientRect();
  return Math.abs(start.x-end.x)>Math.abs(start.y-end.y)
    && Math.abs(box.x+box.width/2-middle.x)<2 && Math.abs(box.y+box.height/2-middle.y)<2;
 });
 await reviewStep.uncheck();
 await edge.waitFor({state:'detached'});
 await label.waitFor({state:'detached'});
});

test('review pass slider supports presets, keyboard, dragging and existing saved values', async t => {
 const {page,getDocument}=await openEditor(t,{width:390,height:844});
 await selectRepository(page,'acme/repo');await page.getByRole('button',{name:'Prompt submission',exact:true}).click();
 await page.getByLabel('Review step',{exact:true}).check();
 const slider=page.getByRole('slider',{name:'Review passes',exact:true});
 const presets=page.getByRole('group',{name:'Review pass presets'});
 assert.deepEqual(await presets.getByRole('button').allTextContents(),['','','','','']);
 for(const icon of ['signal-zero','signal-low','signal-medium','signal-high','signal']) {
  assert.equal(await presets.locator(`svg.lucide-${icon}`).count(),1);
 }
 assert.equal(await page.getByText(/A fresh review agent checks the changes/).count(),0);
 assert.equal(await page.getByRole('spinbutton').count(),0);
 for(const count of [0,1,2,4,8]) {
  await page.getByRole('button',{name:`${count} ${count===1?'pass':'passes'}`,exact:true}).click();
  assert.equal(await slider.inputValue(),String([0,1,2,4,8].indexOf(count)));
 }
 await slider.focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowRight');
 assert.equal(await slider.getAttribute('aria-valuetext'),'1 pass');
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+box.width/4,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:8});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'8 passes');
 await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,8);
 getDocument().workflows[0].reviewCycles=10;
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="review"]').click();
 assert.equal(await slider.getAttribute('aria-valuetext'),'10 passes, saved value; choose a preset to change');
 assert.equal(await presets.getByRole('button',{pressed:true}).count(),0);
 await waitForSaved(page);assert.equal(getDocument().workflows[0].reviewCycles,10);
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
 await page.waitForFunction(()=>document.querySelectorAll('.react-flow__edge-path').length===4);
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
 assert.ok(frames.every(frame=>frame.edges===4&&!frame.hidden), 'nodes and edges stay visible while dragging');
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
 assert.equal(Object.keys(horizontalPositions).length,6);assert.equal(new Set(values.map(p=>p.y)).size,1);
 for(let i=1;i<values.length;i++)assert.ok(values[i].x>values[i-1].x+230);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-right').count(),1);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.deepEqual(getDocument().workflows[0].positions,horizontalPositions);
 await vertical.click();
 await page.getByLabel('Workflow name').fill('Vertical workflow');
 await waitForSaved(page);
 const verticalPositions=steps.map(step=>getDocument().workflows[0].positions[step]);
 assert.equal(new Set(verticalPositions.map(p=>p.x)).size,1);
 for(let i=1;i<verticalPositions.length;i++)assert.ok(verticalPositions[i].y>verticalPositions[i-1].y+100);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-top').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-bottom').count(),1);
 assert.equal(getDocument().workflows[0].merge.enabled,true);
 await page.screenshot({path:join(tmpdir(),'agent00-workflow-layout.png'),fullPage:true});
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
  const { page, getDocument } = await openEditor(t);
  await selectRepository(page, 'acme/repo');
  await page.getByRole('button', { name: 'Scheduled task', exact: true }).click();
  await page.locator('[data-id="task"]').click();
  const complexity = page.getByLabel('Task complexity', { exact: true });
  assert.equal(await complexity.inputValue(), '');
  await complexity.selectOption('advanced');
  await waitForSaved(page);
  assert.equal(getDocument().workflows[0].task.complexity, 'advanced');
  await complexity.selectOption('');
  await waitForSaved(page);
  assert.equal(getDocument().workflows[0].task.complexity, undefined);
});
