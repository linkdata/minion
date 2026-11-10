package main

const adminNavHTML = `{{define "admin-nav"}}<header class="d-flex flex-wrap align-items-center gap-3 px-4 py-3 border-bottom bg-body-tertiary">
  <strong>MINION <span class="text-body-secondary fw-normal">/ operator</span></strong>
  <nav aria-label="Admin"><ul class="nav gap-3">
    <li class="nav-item"><a href="/">Live actions</a></li>
    <li class="nav-item"><a href="/admin/status">Status</a></li>
    <li class="nav-item"><a href="/admin/users">Users</a></li>
    <li class="nav-item"><a href="/admin/repositories">Repositories</a></li>
    <li class="nav-item"><a href="/admin/skills">Skills</a></li>
    <li class="nav-item"><a href="/admin/policy">AI policy</a></li>
    <li class="nav-item"><a href="/admin/config">Configuration</a></li>
    <li class="nav-item"><a href="/admin/usage">Usage</a></li>
  </ul></nav>
  <form class="ms-auto" method="post" action="/admin/logout"><button class="btn btn-outline-light" type="submit">Sign out</button></form>
</header>{{end}}`
