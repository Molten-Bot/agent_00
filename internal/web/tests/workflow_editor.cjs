const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
async function openEditor(t,viewport={width:1280,height:900},initialSettings={workflow_enabled:true}){
 let settings=initialSettings;
 const page=await browser.newPage({viewport});const errors=[];page.on('pageerror',e=>errors.push(e.message));t.after(async()=>{await page.close();assert.deepEqual(errors,[])});
 let doc={version:2,revision:0,workflows:[]};
 await page.addInitScript(()=>{window.EventSource=class{}});
 await page.route('**/*',route=>{const url=new URL(route.request().url());if(url.origin!==new URL(baseURL).origin)return route.abort();
 if(url.pathname==='/api/workflows'){if(route.request().method()==='PUT'){const next=route.request().postDataJSON();if(next.revision!==doc.revision)return route.fulfill({status:409,json:{error:'workflows changed; reload before saving'}});doc={...next,revision:doc.revision+1}}return route.fulfill({json:doc})}
 if(url.pathname==='/api/workflows/preview')return route.fulfill({json:[{item:{number:12,title:'Example issue'},matches:true,reason:'Trigger and filters match'}]});
 if(url.pathname==='/api/library')return route.fulfill({json:{tasks:[{name:'audit',displayName:'Audit repository',prompt:'Audit the repository.'}]}});
 if(url.pathname==='/api/workflows/schedules')return route.fulfill({json:[]});
 if(url.pathname==='/api/workflows/runs')return route.fulfill({json:[]});
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{repos:[{full_name:'acme/repo'},{full_name:'acme/other'},{full_name:'person/project',owner_type:'User'}]}});
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:{required:false,ready:true,state:'ready'}}});
 if(url.pathname==='/api/review-settings'){if(route.request().method()==='POST')settings=route.request().postDataJSON();return route.fulfill({json:{ok:true,settings}})};
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#workflow');await page.locator('#workflow-title').waitFor();return {page,getDocument:()=>doc};
}
async function selectRepository(page, repository) {
 const owner = repository.split('/')[0];
 const owners = page.getByRole('group', {name:'Organizations & people',exact:true});
 const button = owners.getByRole('button').filter({hasText:owner});
 if (await button.getAttribute('aria-pressed') !== 'true') await button.click();
 await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button',{name:repository,exact:true}).click();
}
test('multiple persistent workflows, disabled drafts, policy editing, activation and pause',async t=>{const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.locator('.wf-node').first().waitFor();assert.equal(await page.locator('.wf-node').count(),4);
 await page.getByLabel('Workflow name').fill('Implement ready issues');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].enabled,false);
 await page.getByRole('button',{name:'Enable',exact:true}).click();await page.getByRole('button',{name:'Pause',exact:true}).waitFor();assert.equal(getDocument().workflows[0].enabled,true);await page.getByRole('button',{name:'Pause',exact:true}).click();
 await page.getByRole('button',{name:'Review PRs',exact:true}).click();await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows.length,2);
 await page.getByRole('button',{name:'Preview matches',exact:true}).click();await page.getByText('#12 Example issue',{exact:true}).waitFor();assert.equal(getDocument().workflows.every(w=>!w.enabled),true);
 await page.reload();await selectRepository(page, 'acme/repo');assert.equal(await page.locator('.wf-step').count(),2);await page.locator('.wf-step').first().click();await page.getByLabel('Review step',{exact:true}).check();await page.getByRole('slider',{name:'Review passes',exact:true}).fill('3');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].reviewCycles,4);
 await selectRepository(page, 'acme/other');assert.equal(await page.locator('.wf-step').count(),0);assert.equal(getDocument().workflows.length,2);
 const editor=await page.locator('.wf-editor').boundingBox(),dock=await page.locator('.page-bottom-dock').boundingBox();assert.ok(editor.y+editor.height<=dock.y);});
test('mobile editor exposes repository, canvas and settings without horizontal overflow',async t=>{const {page}=await openEditor(t,{width:390,height:844});await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Implement issues',exact:true}).click();await page.getByLabel('Label',{exact:true}).fill('ready');assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));});

test('scheduled template supports library prompts, interval units, and inline fallback',async t=>{const {page,getDocument}=await openEditor(t);await selectRepository(page, 'acme/repo');await page.getByRole('button',{name:'Scheduled task',exact:true}).click();assert.equal(await page.locator('.wf-node').count(),3);await page.getByRole('button',{name:'days',exact:true}).click();await page.locator('[data-id="task"]').click();await page.getByLabel('Prompt source',{exact:true}).selectOption('audit');await page.getByText('Preview library prompt',{exact:true}).click();await page.getByText('Audit the repository.',{exact:true}).waitFor();await page.getByLabel('Additional context (optional)').fill('Focus on security');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].trigger.event,'schedule');assert.equal(getDocument().workflows[0].task.libraryTaskName,'audit');await page.getByLabel('Prompt source',{exact:true}).selectOption('');await page.getByLabel('Instructions',{exact:true}).fill('Run an inline repository task.');await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');assert.equal(getDocument().workflows[0].task.libraryTaskName,undefined);});

test('retired automation templates create independent disabled workflows for one repository',async t=>{
 const {page,getDocument}=await openEditor(t);
 await selectRepository(page, 'acme/repo');
 await page.getByRole('button',{name:'Assigned issues',exact:true}).click();
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 await page.getByRole('button',{name:'Issue to merge',exact:true}).click();
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
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

test('cached owner picker filters repositories and protects unsaved drafts', async t => {
 const {page}=await openEditor(t);
 assert.equal(await page.getByPlaceholder('owner/repository').count(),0);
 assert.equal(await page.getByRole('button',{name:'Implement issues',exact:true}).isDisabled(),true);
 await page.getByRole('group',{name:'Organizations & people',exact:true}).getByRole('button').filter({hasText:'person'}).click();
 await page.getByRole('button',{name:'person/project',exact:true,pressed:true}).waitFor();
 assert.equal(await page.getByRole('group',{name:'Repositories',exact:true}).getByRole('button').count(),1);
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 await page.getByRole('group',{name:'Organizations & people',exact:true}).getByRole('button').filter({hasText:'person'}).click();
 await page.getByText('Save or discard changes first.',{exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'acme/repo',exact:true}).getAttribute('aria-pressed'),'true');
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
 const save=async()=>{await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft')};
 await page.getByRole('button',{name:'minutes',exact:true}).click();
 await page.getByLabel('Repeat every (minutes)',{exact:true}).fill('7');
 await save();
 assert.equal(getDocument().workflows[0].trigger.intervalHours,7/60);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (minutes)',{exact:true}).inputValue(),'7');
 const slider=page.getByRole('slider',{name:'Repeat interval scale'});
 await slider.scrollIntoViewIfNeeded();const box=await slider.boundingBox();
 await page.mouse.move(box.x+8,box.y+box.height/2);await page.mouse.down();await page.mouse.move(box.x+box.width-8,box.y+box.height/2,{steps:12});await page.mouse.up();
 assert.equal(await slider.getAttribute('aria-valuetext'),'months');
 await page.getByLabel('Repeat every (months)',{exact:true}).fill('2');
 await save();assert.equal(getDocument().workflows[0].trigger.intervalHours,1440);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="trigger"]').click();
 assert.equal(await page.getByLabel('Repeat every (months)',{exact:true}).inputValue(),'2');
 await page.getByRole('slider',{name:'Repeat interval scale'}).focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowRight');
 assert.equal(await page.getByRole('slider').getAttribute('aria-valuetext'),'hours');
 await page.getByRole('button',{name:'weeks',exact:true}).click();await save();assert.equal(getDocument().workflows[0].trigger.intervalHours,336);
 const scale=await page.getByRole('slider').boundingBox(),amount=await page.getByLabel('Repeat every (weeks)',{exact:true}).boundingBox();assert.ok(scale.y+scale.height<amount.y);
 assert.equal(await page.locator('.wf-interval-units svg').count(),5);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 await page.screenshot({path:'/tmp/agent00-schedule-scale-mobile.png',fullPage:true});
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
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
 const w=getDocument().workflows[0];
 assert.equal(w.trigger.event,'app.prompt');assert.equal(w.enabled,false);assert.equal(w.reviewCycles,2);assert.equal(w.merge.enabled,true);
 await page.getByLabel('Review step',{exact:true}).uncheck();
 await page.getByLabel('Merge step',{exact:true}).uncheck();
 assert.equal(await page.locator('.wf-node').count(),3);
 await page.locator('[data-id="task"]').click();
 assert.equal(await page.getByLabel('Prompt source',{exact:true}).count(),0);
 await page.getByLabel('Additional instructions (optional)',{exact:true}).fill('Keep the submitted task scope.');
 await page.getByRole('button',{name:'Save draft',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft');
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
 const save=async()=>{await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft')};
 await save();assert.equal(getDocument().workflows[0].reviewCycles,8);
 getDocument().workflows[0].reviewCycles=10;
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();await page.locator('[data-id="review"]').click();
 assert.equal(await slider.getAttribute('aria-valuetext'),'10 passes, saved value; choose a preset to change');
 assert.equal(await presets.getByRole('button',{pressed:true}).count(),0);
 await save();assert.equal(getDocument().workflows[0].reviewCycles,10);
 await page.getByRole('button',{name:'4 passes',exact:true}).click();await save();assert.equal(getDocument().workflows[0].reviewCycles,4);
 await page.screenshot({path:'/tmp/agent00-review-pass-slider.png',fullPage:true});
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
});

test('automatic layout controls arrange, fit and persist workflow positions', async t => {
 const {page,getDocument}=await openEditor(t);
 const vertical=page.getByRole('button',{name:'Arrange workflow vertically',exact:true});
 const horizontal=page.getByRole('button',{name:'Arrange workflow horizontally',exact:true});
 assert.equal(await vertical.isDisabled(),true);assert.equal(await horizontal.isDisabled(),true);
 await selectRepository(page,'acme/repo');await page.getByRole('button',{name:'Issue to merge',exact:true}).click();
 const save=async()=>{await page.getByRole('button',{name:'Save draft',exact:true}).click();await page.waitForFunction(()=>document.querySelector('.wf-save-state').textContent==='Saved draft')};
 assert.equal(await horizontal.locator('svg').evaluate(svg=>getComputedStyle(svg).fill),'none');
 await horizontal.click();await save();
 const horizontalPositions=structuredClone(getDocument().workflows[0].positions);
 const values=Object.values(horizontalPositions);
 assert.equal(values.length,6);assert.equal(new Set(values.map(p=>p.y)).size,1);
 for(let i=1;i<values.length;i++)assert.ok(values[i].x>values[i-1].x+230);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-right').count(),1);
 await page.reload();await selectRepository(page,'acme/repo');await page.locator('.wf-step').first().click();
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-left').count(),1);
 assert.deepEqual(getDocument().workflows[0].positions,horizontalPositions);
 await vertical.click();await save();
 const verticalPositions=Object.values(getDocument().workflows[0].positions);
 assert.equal(new Set(verticalPositions.map(p=>p.x)).size,1);
 for(let i=1;i<verticalPositions.length;i++)assert.ok(verticalPositions[i].y>verticalPositions[i-1].y+100);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-top').count(),1);
 assert.equal(await page.locator('[data-id="task"] .react-flow__handle-bottom').count(),1);
 assert.equal(getDocument().workflows[0].merge.enabled,true);
 await page.screenshot({path:'/tmp/agent00-workflow-layout.png',fullPage:true});
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

test('Workflow maximizes with navigation available and restores on leaving', async t => {
 const {page}=await openEditor(t,{width:1600,height:900});
 const panel=page.locator('#workflow-display');
 const maximize=page.getByRole('button',{name:'Maximize Workflow'});
 const restore=page.getByRole('button',{name:'Restore Workflow size'});
 await selectRepository(page,'acme/repo');
 await page.getByRole('button',{name:'Implement issues',exact:true}).click();
 const workflowName=page.getByRole('textbox',{name:'Workflow name'});
 await workflowName.fill('Unsaved workflow');
 const defaultSize=await panel.boundingBox();

 await maximize.click();
 assert.equal(await restore.getAttribute('aria-expanded'),'true');
 const expanded=await panel.boundingBox();
 const dock=await page.locator('.page-bottom-dock').boundingBox();
 assert.equal(expanded.x,0);
 assert.equal(expanded.y,0);
 assert.equal(expanded.width,1600);
 assert.ok(expanded.height>defaultSize.height);
 assert.ok(expanded.y+expanded.height<=dock.y);
 await restore.click();
 assert.deepEqual(await panel.boundingBox(),defaultSize);

 await maximize.click();
 await page.keyboard.press('Escape');
 await maximize.waitFor();
 assert.deepEqual(await panel.boundingBox(),defaultSize);

 await maximize.click();
 await page.locator('#prompt-mode-builder').click();
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
